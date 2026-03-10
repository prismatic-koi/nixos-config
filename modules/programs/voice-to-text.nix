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
      (torch.override {
        rocmSupport = true;
      })
    ]
  );

  # Python script for voice-to-text functionality
  # Using openai-whisper with ROCm GPU support
  voiceToTextScript = pkgs.writeScriptBin "voice-to-text" /* python */ ''
    #!${pythonWithWhisper}/bin/python3
    import os
    import sys
    import time
    import json
    import subprocess
    import tempfile
    from pathlib import Path

    try:
        import whisper
    except ImportError as e:
        print(f"Error: Missing required Python package: {e}", file=sys.stderr)
        subprocess.run(["notify-send", "Voice-to-Text Error", f"Missing Python package: {e}"])
        sys.exit(1)

    # Configuration
    CONFIG_DIR = Path.home() / ".config" / "voice-to-text"
    CONFIG_FILE = CONFIG_DIR / "config.json"
    STATE_FILE = CONFIG_DIR / "state.json"
    CACHE_DIR = Path.home() / ".cache" / "voice-to-text" / "models"
    LOG_FILE = CONFIG_DIR / "voice-to-text.log"

    # Default configuration
    DEFAULT_CONFIG = {
        "model": "${cfg.model}",
        "device": "cuda",  # Use CUDA/ROCm (PyTorch detects ROCm as CUDA)
        "sample_rate": 16000,
        "channels": 1,
        "backend": "${cfg.backend}",
        "notification_sound": ${if cfg.notificationSound then "True" else "False"},
        "language": ${if cfg.language == "" then "None" else "\"${cfg.language}\""},
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

    def notify(title, message, urgency="normal"):
        """Send desktop notification."""
        subprocess.run(["notify-send", "-u", urgency, title, message])

    def play_notification_sound():
        """Play a short notification beep."""
        config = load_config()
        if config["notification_sound"]:
            # Use paplay with a simple beep
            subprocess.run(["paplay", "/usr/share/sounds/freedesktop/stereo/message.oga"], 
                         stderr=subprocess.DEVNULL, check=False)

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
        
        # Start recording in background
        try:
            notify("Voice-to-Text", "Recording started...")
            play_notification_sound()
            
            # Save state
            set_state(True, temp_path)
            
            # Start recording with pw-record (PipeWire)
            # Record directly to WAV file (pw-record can output WAV format)
            process = subprocess.Popen(
                ["pw-record", "--format=s16", f"--rate={config['sample_rate']}", 
                 f"--channels={config['channels']}", temp_path],
                stderr=subprocess.DEVNULL
            )
            
            # Save PID for later termination
            pid_file = STATE_FILE.parent / "recording.pid"
            with open(pid_file, 'w') as f:
                f.write(str(process.pid))
            
        except Exception as e:
            notify("Voice-to-Text Error", f"Failed to start recording: {e}", "critical")
            set_state(False)
            print(f"Error starting recording: {e}", file=sys.stderr)
            sys.exit(1)

    def stop_recording_and_transcribe():
        """Stop recording and transcribe."""
        overall_start = time.time()
        print(f"[{time.time():.2f}] stop_recording_and_transcribe() called", file=sys.stderr)
        
        config = load_config()
        state = get_state()
        
        if not state["recording"]:
            print("Not recording", file=sys.stderr)
            return
        
        try:
            # Kill recording process
            kill_start = time.time()
            pid_file = STATE_FILE.parent / "recording.pid"
            if pid_file.exists():
                with open(pid_file, 'r') as f:
                    pid = int(f.read().strip())
                try:
                    os.kill(pid, 15)  # SIGTERM
                    time.sleep(0.5)  # Give it time to finish writing
                except ProcessLookupError:
                    pass  # Process already dead
                pid_file.unlink()
            print(f"Kill process time: {time.time() - kill_start:.2f}s", file=sys.stderr)
            
            temp_path = state["temp_file"]
            set_state(False)
            
            if not temp_path or not os.path.exists(temp_path):
                notify("Voice-to-Text Error", "No recording found", "critical")
                return
            
            # Check if file has content
            check_start = time.time()
            if os.path.getsize(temp_path) < 1000:  # Less than 1KB
                notify("Voice-to-Text", "Recording too short", "normal")
                os.unlink(temp_path)
                return
            print(f"File check time: {time.time() - check_start:.2f}s", file=sys.stderr)
            
            notify_start = time.time()
            notify("Voice-to-Text", "Transcribing...", "normal")
            print(f"Notify time: {time.time() - notify_start:.2f}s", file=sys.stderr)
            
            # Transcribe
            transcribe_start = time.time()
            text = transcribe_audio(temp_path, config)
            print(f"transcribe_audio() took: {time.time() - transcribe_start:.2f}s", file=sys.stderr)
            
            # Cleanup
            os.unlink(temp_path)
            
            if text:
                # Insert text using configured backend
                insert_start = time.time()
                insert_text(text, config["backend"])
                print(f"Insert text time: {time.time() - insert_start:.2f}s", file=sys.stderr)
                
                notify_start = time.time()
                notify("Voice-to-Text", f"Transcribed: {text[:50]}...", "normal")
                print(f"Final notify time: {time.time() - notify_start:.2f}s", file=sys.stderr)
                
                sound_start = time.time()
                play_notification_sound()
                print(f"Sound time: {time.time() - sound_start:.2f}s", file=sys.stderr)
            else:
                notify("Voice-to-Text Error", "Transcription failed", "critical")
            
            print(f"TOTAL stop_recording_and_transcribe() time: {time.time() - overall_start:.2f}s", file=sys.stderr)
            
        except Exception as e:
            notify("Voice-to-Text Error", f"Error: {e}", "critical")
            print(f"Error: {e}", file=sys.stderr)
            set_state(False)
            sys.exit(1)

    def transcribe_audio(audio_path, config):
        """Transcribe audio file using openai-whisper with GPU support."""
        try:
            import torch
            import time
            
            # Open log file for timing info
            log_f = open(LOG_FILE, 'a')
            
            start_time = time.time()
            print(f"[T+0.00] Starting transcribe_audio", file=sys.stderr)
            
            # Ensure cache directory exists
            CACHE_DIR.mkdir(parents=True, exist_ok=True)
            
            # Set download directory for models
            os.environ["WHISPER_CACHE_DIR"] = str(CACHE_DIR)
            
            # Check GPU availability
            device = config["device"]
            if device == "cuda" and not torch.cuda.is_available():
                msg = "Warning: CUDA/ROCm not available, falling back to CPU"
                print(msg, file=sys.stderr)
                print(msg, file=log_f)
                notify("Voice-to-Text", "GPU not available, using CPU", "normal")
                device = "cpu"
            else:
                msg = f"Using device: {device}"
                print(msg, file=sys.stderr)
                print(msg, file=log_f)
                if device == "cuda":
                    gpu_msg = f"GPU: {torch.cuda.get_device_name(0)}"
                    print(gpu_msg, file=sys.stderr)
                    print(gpu_msg, file=log_f)
            
            # Load model (models are cached after first download)
            print(f"[T+{time.time()-start_time:.2f}] Loading model...", file=sys.stderr)
            load_start = time.time()
            model = whisper.load_model(config["model"], device=device)
            load_time = time.time() - load_start
            load_msg = f"Model load time: {load_time:.2f}s"
            print(load_msg, file=sys.stderr)
            print(load_msg, file=log_f)
            
            # Transcribe
            print(f"[T+{time.time()-start_time:.2f}] Starting transcription...", file=sys.stderr)
            transcribe_start = time.time()
            language = config["language"] if config["language"] != "None" else None
            
            print(f"[T+{time.time()-start_time:.2f}] Calling model.transcribe()...", file=sys.stderr)
            result = model.transcribe(
                audio_path,
                language=language,
                fp16=(device == "cuda")  # Use half precision on GPU
            )
            print(f"[T+{time.time()-start_time:.2f}] model.transcribe() returned", file=sys.stderr)
            
            transcribe_time = time.time() - transcribe_start
            trans_msg = f"Transcription time: {transcribe_time:.2f}s"
            print(trans_msg, file=sys.stderr)
            print(trans_msg, file=log_f)
            
            # Extract text from result
            text = result["text"]
            
            total_time = time.time() - start_time
            total_msg = f"Total time: {total_time:.2f}s"
            print(total_msg, file=sys.stderr)
            print(total_msg, file=log_f)
            print(f"Text: {text}", file=log_f)
            print("---", file=log_f)
            log_f.close()
            
            return text.strip()
            
        except Exception as e:
            print(f"Transcription error: {e}", file=sys.stderr)
            import traceback
            traceback.print_exc()
            return None

    def insert_text(text, backend):
        """Insert text using specified backend."""
        try:
            if backend == "wtype":
                subprocess.run(["wtype", text], check=True)
            elif backend == "ydotool":
                subprocess.run(["ydotool", "type", text], check=True)
            elif backend == "clipboard":
                # Copy to clipboard
                subprocess.run(["wl-copy"], input=text.encode(), check=True)
                notify("Voice-to-Text", "Text copied to clipboard", "normal")
            elif backend == "auto":
                # Try wtype first, then ydotool, then clipboard
                try:
                    subprocess.run(["wtype", text], check=True, stderr=subprocess.DEVNULL)
                except:
                    try:
                        subprocess.run(["ydotool", "type", text], check=True, stderr=subprocess.DEVNULL)
                    except:
                        subprocess.run(["wl-copy"], input=text.encode(), check=True)
                        notify("Voice-to-Text", "Text copied to clipboard (fallback)", "normal")
        except Exception as e:
            notify("Voice-to-Text Error", f"Failed to insert text: {e}", "critical")
            print(f"Error inserting text: {e}", file=sys.stderr)

    def cancel_recording():
        """Cancel ongoing recording."""
        state = get_state()
        
        if not state["recording"]:
            print("Not recording", file=sys.stderr)
            return
        
        # Kill recording process
        pid_file = STATE_FILE.parent / "recording.pid"
        if pid_file.exists():
            with open(pid_file, 'r') as f:
                pid = int(f.read().strip())
            try:
                os.kill(pid, 15)  # SIGTERM
            except ProcessLookupError:
                pass
            pid_file.unlink()
        
        # Cleanup
        if state["temp_file"] and os.path.exists(state["temp_file"]):
            os.unlink(state["temp_file"])
        
        set_state(False)
        notify("Voice-to-Text", "Recording cancelled", "normal")

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
      enable = lib.mkEnableOption "voice-to-text with OpenAI Whisper" // {
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
      # Install required system packages (but not Python env to avoid conflicts)
      home.packages = [
        voiceToTextScript # This uses pythonWithWhisper in its shebang
        pkgs.wtype
        pkgs.wl-clipboard
        pkgs.pipewire
        pkgs.libnotify
        pkgs.pulseaudio # for paplay notification sound
      ];

      # Add Hyprland keybinding
      wayland.windowManager.hyprland.settings.bind = [
        # Hold to talk: press to start recording, release to transcribe
        "${cfg.keybind}, exec, voice-to-text toggle"
        # Cancel recording
        "${cfg.keybind} SHIFT, exec, voice-to-text cancel"
      ];

      # Persist configuration and model cache
      home.persistence."/persist" = {
        directories = [
          ".config/voice-to-text"
          ".cache/voice-to-text" # Model cache
        ];
      };

      # Environment variables for ROCm
      home.sessionVariables = {
        # Enable ROCm for faster-whisper
        HSA_OVERRIDE_GFX_VERSION = "10.3.0"; # For RX 6950 XT
        ROCM_PATH = "${pkgs.rocmPackages.clr}";
      };
    };

    # Enable PipeWire
    services.pipewire = {
      enable = true;
      audio.enable = true;
      pulse.enable = true;
    };
  };
}
