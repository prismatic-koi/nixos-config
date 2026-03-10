{
  config,
  pkgs,
  lib,
  ...
}:
let
  cfg = config.nx.programs.voiceToText;
  homeDir = config.home-manager.users.${config.nx.username}.home.homeDirectory;

  # Python environment with openai-whisper and ROCm-enabled PyTorch
  pythonWithWhisper = pkgs.python313.withPackages (
    ps: with ps; [
      openai-whisper
      # Use PyTorch with ROCm support explicitly
      (torch.override { rocmSupport = true; })
    ]
  );

  # Daemon that keeps the Whisper model loaded in memory
  voiceToTextDaemon = pkgs.writeScriptBin "voice-to-text-daemon" ''
    #!${pythonWithWhisper}/bin/python3
    import os
    import sys
    import socket
    import json
    import time
    import threading
    from pathlib import Path

    # Import heavy dependencies once at startup
    import torch
    import whisper

    # Configuration
    CONFIG_DIR = Path.home() / ".config" / "voice-to-text"
    SOCKET_PATH = Path.home() / ".cache" / "voice-to-text" / "daemon.sock"
    CACHE_DIR = Path.home() / ".cache" / "voice-to-text" / "models"

    # Ensure directories exist
    CONFIG_DIR.mkdir(parents=True, exist_ok=True)
    SOCKET_PATH.parent.mkdir(parents=True, exist_ok=True)
    CACHE_DIR.mkdir(parents=True, exist_ok=True)

    # Set cache directory
    os.environ["WHISPER_CACHE_DIR"] = str(CACHE_DIR)

    print(f"[Daemon] Starting voice-to-text daemon", file=sys.stderr)
    print(f"[Daemon] Loading Whisper model...", file=sys.stderr)

    # Load model once at startup
    device = "cuda" if torch.cuda.is_available() else "cpu"
    if device == "cuda":
        print(f"[Daemon] Using GPU: {torch.cuda.get_device_name(0)}", file=sys.stderr)
    else:
        print(f"[Daemon] Using CPU", file=sys.stderr)

    model = whisper.load_model("${cfg.model}", device=device)
    print(f"[Daemon] Model loaded successfully", file=sys.stderr)

    def transcribe_audio(audio_path):
        """Transcribe audio file using pre-loaded model."""
        try:
            start = time.time()
            result = model.transcribe(
                audio_path,
                language=${if cfg.language == "" then "None" else "\"${cfg.language}\""},
                fp16=(device == "cuda")
            )
            elapsed = time.time() - start
            print(f"[Daemon] Transcribed in {elapsed:.2f}s", file=sys.stderr)
            return {"success": True, "text": result["text"].strip(), "time": elapsed}
        except Exception as e:
            print(f"[Daemon] Transcription error: {e}", file=sys.stderr)
            return {"success": False, "error": str(e)}

    def handle_client(conn):
        """Handle client request."""
        try:
            data = conn.recv(4096).decode('utf-8')
            request = json.loads(data)
            
            if request["command"] == "transcribe":
                audio_path = request["audio_path"]
                result = transcribe_audio(audio_path)
                conn.sendall(json.dumps(result).encode('utf-8'))
            elif request["command"] == "ping":
                conn.sendall(json.dumps({"success": True, "status": "ready"}).encode('utf-8'))
            elif request["command"] == "shutdown":
                conn.sendall(json.dumps({"success": True}).encode('utf-8'))
                return False
            else:
                conn.sendall(json.dumps({"success": False, "error": "Unknown command"}).encode('utf-8'))
        except Exception as e:
            print(f"[Daemon] Error handling client: {e}", file=sys.stderr)
            try:
                conn.sendall(json.dumps({"success": False, "error": str(e)}).encode('utf-8'))
            except:
                pass
        return True

    def main():
        # Remove old socket if it exists
        if SOCKET_PATH.exists():
            SOCKET_PATH.unlink()
        
        # Create Unix socket
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.bind(str(SOCKET_PATH))
        sock.listen(1)
        
        print(f"[Daemon] Listening on {SOCKET_PATH}", file=sys.stderr)
        print(f"[Daemon] Ready to accept transcription requests", file=sys.stderr)
        
        try:
            while True:
                conn, _ = sock.accept()
                if not handle_client(conn):
                    conn.close()
                    break
                conn.close()
        finally:
            sock.close()
            if SOCKET_PATH.exists():
                SOCKET_PATH.unlink()
            print(f"[Daemon] Shutdown complete", file=sys.stderr)

    if __name__ == "__main__":
        main()
  '';

  # Client script that communicates with the daemon
  voiceToTextClient = pkgs.writeScriptBin "voice-to-text" ''
    #!${pkgs.python313}/bin/python3
    import os
    import sys
    import socket
    import json
    import time
    import subprocess
    import tempfile
    from pathlib import Path

    # Configuration
    CONFIG_DIR = Path.home() / ".config" / "voice-to-text"
    CONFIG_FILE = CONFIG_DIR / "config.json"
    STATE_FILE = CONFIG_DIR / "state.json"
    SOCKET_PATH = Path.home() / ".cache" / "voice-to-text" / "daemon.sock"
    NOTIFICATION_ID = 999888  # Fixed ID for our notifications

    # Default configuration
    DEFAULT_CONFIG = {
        "backend": "${cfg.backend}",
        "notification_sound": ${if cfg.notificationSound then "True" else "False"},
    }

    def load_config():
        """Load configuration from file or create default."""
        CONFIG_DIR.mkdir(parents=True, exist_ok=True)
        if CONFIG_FILE.exists():
            with open(CONFIG_FILE, 'r') as f:
                config = {**DEFAULT_CONFIG, **json.load(f)}
        else:
            config = DEFAULT_CONFIG
            with open(CONFIG_FILE, 'w') as f:
                json.dump(config, f, indent=2)
        return config

    def get_state():
        """Get current recording state."""
        if STATE_FILE.exists():
            with open(STATE_FILE, 'r') as f:
                return json.load(f)
        return {"recording": False, "temp_file": None}

    def set_state(recording, temp_file=None):
        """Set recording state."""
        with open(STATE_FILE, 'w') as f:
            json.dump({"recording": recording, "temp_file": temp_file}, f)

    def notify(title, message, urgency="normal", replace=True, icon=None, expire_time=None):
        """Send desktop notification, optionally replacing previous one."""
        cmd = ["notify-send", "-u", urgency]
        if replace:
            cmd.extend(["-r", str(NOTIFICATION_ID)])
        if icon:
            cmd.extend(["-i", icon])
        if expire_time is not None:
            cmd.extend(["-t", str(expire_time)])
        cmd.extend([title, message])
        subprocess.run(cmd)

    def play_notification_sound():
        """Play a short notification beep."""
        config = load_config()
        if config["notification_sound"]:
            subprocess.run(["paplay", "${pkgs.sound-theme-freedesktop}/share/sounds/freedesktop/stereo/message.oga"], 
                         stderr=subprocess.DEVNULL, check=False)

    def play_start_sound():
        """Play sound for recording start (rising tone)."""
        config = load_config()
        if config["notification_sound"]:
            subprocess.run(["paplay", "${pkgs.kdePackages.ocean-sound-theme}/share/sounds/ocean/stereo/power-plug.oga"], 
                         stderr=subprocess.DEVNULL, check=False)

    def play_end_sound():
        """Play sound for recording end (falling tone)."""
        config = load_config()
        if config["notification_sound"]:
            subprocess.run(["paplay", "${pkgs.kdePackages.ocean-sound-theme}/share/sounds/ocean/stereo/power-unplug.oga"], 
                         stderr=subprocess.DEVNULL, check=False)

    def send_to_daemon(command, **kwargs):
        """Send command to daemon and get response."""
        try:
            sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            sock.connect(str(SOCKET_PATH))
            request = {"command": command, **kwargs}
            sock.sendall(json.dumps(request).encode('utf-8'))
            response = sock.recv(4096).decode('utf-8')
            sock.close()
            return json.loads(response)
        except FileNotFoundError:
            notify("Voice-to-Text Error", "Daemon not running. Start with: systemctl --user start voice-to-text.service", "critical", replace=False)
            return {"success": False, "error": "Daemon not running"}
        except Exception as e:
            notify("Voice-to-Text Error", f"Communication error: {e}", "critical", replace=False)
            return {"success": False, "error": str(e)}

    def start_recording():
        """Start recording audio."""
        config = load_config()
        state = get_state()
        
        if state["recording"]:
            print("Already recording", file=sys.stderr)
            return
        
        # Create temporary file for audio
        temp_file = tempfile.NamedTemporaryFile(suffix=".wav", delete=False)
        temp_path = temp_file.name
        temp_file.close()
        
        try:
            # Play rising tone to indicate recording started
            play_start_sound()
            
            # Save state
            set_state(True, temp_path)
            
            # Start recording with pw-record (PipeWire)
            process = subprocess.Popen(
                ["pw-record", "--format=s16", "--rate=16000", 
                 "--channels=1", temp_path],
                stderr=subprocess.DEVNULL
            )
            
            # Save PID for later termination
            pid_file = STATE_FILE.parent / "recording.pid"
            with open(pid_file, 'w') as f:
                f.write(str(process.pid))
            
        except Exception as e:
            notify("Voice-to-Text Error", f"Failed to start recording: {e}", "critical", replace=False)
            set_state(False)
            sys.exit(1)

    def stop_recording_and_transcribe():
        """Stop recording and transcribe using daemon."""
        config = load_config()
        state = get_state()
        
        if not state["recording"]:
            print("Not recording", file=sys.stderr)
            return
        
        try:
            # Kill recording process
            pid_file = STATE_FILE.parent / "recording.pid"
            if pid_file.exists():
                with open(pid_file, 'r') as f:
                    pid = int(f.read().strip())
                try:
                    os.kill(pid, 15)  # SIGTERM
                    time.sleep(0.5)
                except ProcessLookupError:
                    pass
                pid_file.unlink()
            
            temp_path = state["temp_file"]
            set_state(False)
            
            if not temp_path or not os.path.exists(temp_path):
                notify("Voice-to-Text Error", "No recording found", "critical", replace=False)
                return
            
            if os.path.getsize(temp_path) < 1000:
                notify("Voice-to-Text", "Recording too short", "normal", replace=False, expire_time=2000)
                os.unlink(temp_path)
                return
            
            # Send to daemon for transcription (no notification - it's fast!)
            result = send_to_daemon("transcribe", audio_path=temp_path)
            
            # Cleanup
            os.unlink(temp_path)
            
            if result["success"]:
                text = result["text"]
                insert_text(text, config["backend"])
                # Brief success notification that expires after 3 seconds
                if len(text) > 50:
                    display_text = text[:50] + "..."
                else:
                    display_text = text
                notify("Voice-to-Text", display_text, "normal", replace=False, icon="emblem-ok-symbolic", expire_time=3000)
                play_end_sound()  # Play falling tone on success
            else:
                notify("Voice-to-Text Error", f"Transcription failed: {result.get('error', 'Unknown')}", "critical", replace=False)
            
        except Exception as e:
            notify("Voice-to-Text Error", f"Error: {e}", "critical", replace=False)
            set_state(False)
            sys.exit(1)

    def insert_text(text, backend):
        """Insert text using specified backend."""
        try:
            if backend == "wtype":
                subprocess.run(["wtype", text], check=True)
            elif backend == "ydotool":
                subprocess.run(["ydotool", "type", text], check=True)
            elif backend == "clipboard":
                subprocess.run(["wl-copy"], input=text.encode(), check=True)
                # Don't show extra notification - already shown in stop_recording_and_transcribe
            elif backend == "auto":
                try:
                    subprocess.run(["wtype", text], check=True, stderr=subprocess.DEVNULL)
                except:
                    try:
                        subprocess.run(["ydotool", "type", text], check=True, stderr=subprocess.DEVNULL)
                    except:
                        subprocess.run(["wl-copy"], input=text.encode(), check=True)
                        # Don't show notification here - shown at end
        except Exception as e:
            notify("Voice-to-Text Error", f"Failed to insert text: {e}", "critical", replace=False)

    def cancel_recording():
        """Cancel ongoing recording."""
        state = get_state()
        
        if not state["recording"]:
            return
        
        pid_file = STATE_FILE.parent / "recording.pid"
        if pid_file.exists():
            with open(pid_file, 'r') as f:
                pid = int(f.read().strip())
            try:
                os.kill(pid, 15)
            except ProcessLookupError:
                pass
            pid_file.unlink()
        
        if state["temp_file"] and os.path.exists(state["temp_file"]):
            os.unlink(state["temp_file"])
        
        set_state(False)
        notify("Voice-to-Text", "Cancelled", "normal", replace=False, icon="process-stop", expire_time=2000)

    def main():
        if len(sys.argv) < 2:
            print("Usage: voice-to-text {toggle|start|stop|cancel|status}")
            sys.exit(1)
        
        command = sys.argv[1]
        
        if command == "toggle":
            state = get_state()
            if state["recording"]:
                stop_recording_and_transcribe()
            else:
                start_recording()
        elif command == "start":
            start_recording()
        elif command == "stop":
            stop_recording_and_transcribe()
        elif command == "cancel":
            cancel_recording()
        elif command == "status":
            state = get_state()
            print("Recording" if state["recording"] else "Idle")
        else:
            print(f"Unknown command: {command}")
            sys.exit(1)

    if __name__ == "__main__":
        main()
  '';

in
{
  options = {
    nx.programs.voiceToText = {
      enable = lib.mkEnableOption "voice-to-text with OpenAI Whisper daemon" // {
        default = false;
      };

      model = lib.mkOption {
        type = lib.types.enum [
          "tiny"
          "base"
          "small"
          "medium"
          "large-v2"
          "large-v3"
        ];
        default = "base";
        description = "Whisper model size (tiny=fastest, large-v3=most accurate)";
      };

      backend = lib.mkOption {
        type = lib.types.enum [
          "wtype"
          "ydotool"
          "clipboard"
          "auto"
        ];
        default = "auto";
        description = "Text injection backend (auto tries wtype -> ydotool -> clipboard)";
      };

      keybind = lib.mkOption {
        type = lib.types.str;
        default = "SUPER, V";
        description = "Keybind for toggle voice recording (hold to talk, release to transcribe)";
      };

      notificationSound = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Play notification sound on recording start/stop";
      };

      language = lib.mkOption {
        type = lib.types.str;
        default = "";
        description = "Language for transcription (empty for auto-detect, or 'en', 'es', 'fr', etc.)";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    home-manager.users.${config.nx.username} = {
      # Install packages
      home.packages = [
        voiceToTextClient # User-facing client
        pkgs.wtype
        pkgs.wl-clipboard
        pkgs.pipewire
        pkgs.libnotify
        pkgs.pulseaudio # for paplay
        pkgs.kdePackages.ocean-sound-theme # KDE Ocean sound theme
      ];

      # Systemd user service for the daemon
      systemd.user.services.voice-to-text = {
        Unit = {
          Description = "Voice-to-Text Whisper daemon";
          After = [ "pipewire.service" ];
          Wants = [ "pipewire.service" ];
        };

        Service = {
          Type = "simple";
          ExecStart = "${voiceToTextDaemon}/bin/voice-to-text-daemon";
          Restart = "on-failure";
          RestartSec = 5;
          Environment = [
            "HSA_OVERRIDE_GFX_VERSION=10.3.0"
            "ROCM_PATH=${pkgs.rocmPackages.clr}"
          ];
        };

        Install = {
          WantedBy = [ "default.target" ];
        };
      };

      # Hyprland keybinding
      wayland.windowManager.hyprland.settings.bind = [
        "${cfg.keybind}, exec, voice-to-text toggle"
        "${cfg.keybind} SHIFT, exec, voice-to-text cancel"
      ];

      # Persist configuration and cache
      home.persistence."/persist" = {
        directories = [
          ".config/voice-to-text"
          ".cache/voice-to-text"
        ];
      };
    };
  };
}
