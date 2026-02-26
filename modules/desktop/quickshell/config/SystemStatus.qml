import Quickshell
import Quickshell.Wayland
import Quickshell.Io
import QtQuick

// System status bar — bottom-right pill with network, VPN, battery, power
// profile, and systemd unit health indicators.
//
// Layout (left → right inside a single pill):
//
//   [ network ] | [ vpn ] | [ battery ] | [ power profile ] | [ systemd ]
//
// Visibility follows the same show/hide pattern as other bars: hidden by
// default, toggled via showBar() / hideBar() called from shell.qml on
// Hyprland custom events.

PanelWindow {
    id: root

    // -- configuration --
    readonly property int barHeight: 40
    readonly property int barMargin: 30
    readonly property int barRadius: 5
    readonly property int hPad: 14
    readonly property int fontSize: 18
    readonly property int itemSpacing: 10
    readonly property int dividerW: 1
    readonly property int dividerH: 20
    readonly property int groupSpacing: 14

    // -- state: network --
    property string networkText: "no network"
    property bool networkConnected: false
    property bool networkIsWifi: false

    // -- state: VPN --
    property bool vpnConnected: false

    // -- state: battery --
    property string batteryText: ""
    property bool hasBattery: false
    property bool batteryCharging: false

    // -- state: power profile --
    property string powerProfile: "balanced"

    // -- state: systemd --
    property bool systemdOk: true
    property int systemdFailed: 0

    // ── Network polling (every 5 s) ───────────────────────────────────────────
    Process {
        id: netProc
        command: [
            "sh", "-c",
            // Try Wi-Fi via nmcli (SSID + signal), fall back to ethernet iface name
            "wifi=$(nmcli -t -f active,ssid,signal dev wifi 2>/dev/null | awk -F: '/^yes:/{printf \"%s:%s\", $2, $3; exit}'); " +
            "if [ -n \"$wifi\" ]; then echo \"wifi:$wifi\"; " +
            "else eth=$(nmcli -t -f device,type,state dev 2>/dev/null | awk -F: '$2==\"ethernet\" && $3==\"connected\"{print $1; exit}'); " +
            "[ -n \"$eth\" ] && echo \"eth:$eth\" || echo 'none:'; fi"
        ]
        stdout: StdioCollector {
            onStreamFinished: {
                var out = this.text.trim();
                if (out.startsWith("wifi:")) {
                    // format: wifi:<ssid>:<signal>
                    var rest = out.substring(5);
                    var lastColon = rest.lastIndexOf(":");
                    var ssid = lastColon > 0 ? rest.substring(0, lastColon) : rest;
                    var sig = lastColon > 0 ? parseInt(rest.substring(lastColon + 1), 10) : 0;
                    var strength = isNaN(sig) ? "" : (" (" + sig + "%)");
                    root.networkText = (ssid.length > 0 ? ssid : "Wi-Fi") + strength;
                    root.networkConnected = true;
                    root.networkIsWifi = true;
                } else if (out.startsWith("eth:")) {
                    var iface = out.substring(4).trim();
                    root.networkText = iface.length > 0 ? iface : "ethernet";
                    root.networkConnected = true;
                    root.networkIsWifi = false;
                } else {
                    root.networkText = "no network";
                    root.networkConnected = false;
                    root.networkIsWifi = false;
                }
            }
        }
    }

    Timer {
        interval: 5000
        repeat: true
        running: true
        onTriggered: netProc.running = true
    }

    Timer {
        interval: 150
        repeat: false
        running: true
        onTriggered: netProc.running = true
    }

    // ── VPN polling (every 5 s) ───────────────────────────────────────────────
    Process {
        id: vpnProc
        command: [
            "sh", "-c",
            // WireGuard interfaces always report 'state UNKNOWN' in ip-link, so
            // use nmcli which correctly reports 'connected (externally)' for wgnord.
            "nmcli -t -f device,type,state dev 2>/dev/null | grep -q '^wgnord:wireguard:connected' && echo connected || echo disconnected"
        ]
        stdout: StdioCollector {
            onStreamFinished: {
                root.vpnConnected = (this.text.trim() === "connected");
            }
        }
    }

    Timer {
        interval: 5000
        repeat: true
        running: true
        onTriggered: vpnProc.running = true
    }

    Timer {
        interval: 200
        repeat: false
        running: true
        onTriggered: vpnProc.running = true
    }

    // ── Battery polling (every 5 s) ───────────────────────────────────────────
    Process {
        id: batProc
        command: [
            "sh", "-c",
            "bat=/sys/class/power_supply/BAT0; [ -d \"$bat\" ] || exit 0; " +
            "cap=$(cat \"$bat/capacity\" 2>/dev/null); " +
            "status=$(cat \"$bat/status\" 2>/dev/null); " +
            "echo \"${cap}:${status}\""
        ]
        stdout: StdioCollector {
            onStreamFinished: {
                var out = this.text.trim();
                if (out.length === 0) {
                    root.hasBattery = false;
                    return;
                }
                root.hasBattery = true;
                var parts = out.split(":");
                var cap = parseInt(parts[0], 10) || 0;
                var status = parts[1] || "";
                root.batteryCharging = (status === "Charging" || status === "Full");
                root.batteryText = cap + "%";
            }
        }
    }

    Timer {
        interval: 5000
        repeat: true
        running: true
        onTriggered: batProc.running = true
    }

    Timer {
        interval: 250
        repeat: false
        running: true
        onTriggered: batProc.running = true
    }

    // ── Power profile polling (every 10 s) ────────────────────────────────────
    Process {
        id: powerProc
        command: ["sh", "-c", "powerprofilesctl get 2>/dev/null || echo balanced"]
        stdout: StdioCollector {
            onStreamFinished: {
                root.powerProfile = this.text.trim();
            }
        }
    }

    Timer {
        interval: 10000
        repeat: true
        running: true
        onTriggered: powerProc.running = true
    }

    Timer {
        interval: 300
        repeat: false
        running: true
        onTriggered: powerProc.running = true
    }

    // ── Systemd failed-units polling (every 10 s) ─────────────────────────────
    Process {
        id: systemdProc
        command: [
            "sh", "-c",
            "failed=$(systemctl --failed --no-legend 2>/dev/null | wc -l); " +
            "user_failed=$(systemctl --user --failed --no-legend 2>/dev/null | wc -l); " +
            "echo $((failed + user_failed))"
        ]
        stdout: StdioCollector {
            onStreamFinished: {
                var n = parseInt(this.text.trim(), 10);
                root.systemdFailed = isNaN(n) ? 0 : n;
                root.systemdOk = (root.systemdFailed === 0);
            }
        }
    }

    Timer {
        interval: 10000
        repeat: true
        running: true
        onTriggered: systemdProc.running = true
    }

    Timer {
        interval: 350
        repeat: false
        running: true
        onTriggered: systemdProc.running = true
    }

    // ── Helper functions ──────────────────────────────────────────────────────

    // Power profile icon — returns a Nerd Font codepoint
    function powerIcon(profile) {
        if (profile === "performance") return "󱐋";
        if (profile === "power-saver") return "󰌪";
        return "󰾅";
    }

    // Battery icon — tiered icons matching the waybar script
    function batteryIcon(text, charging) {
        if (charging) return "󰂄";
        var cap = parseInt(text, 10);
        if (isNaN(cap)) return "󰂑";
        if (cap >= 95) return "󰁹";
        if (cap >= 80) return "󰂂";
        if (cap >= 60) return "󰂀";
        if (cap >= 40) return "󰁾";
        if (cap >= 20) return "󰁻";
        return "󰁺";
    }

    // Battery colour — green when charging, orange when low, otherwise foreground
    function batteryColor(text, charging) {
        if (charging) return Theme.green;
        var cap = parseInt(text, 10);
        if (!isNaN(cap) && cap <= 20) return Theme.orange;
        return Theme.foreground;
    }

    // ── Slide animation ───────────────────────────────────────────────────────
    property bool showing: false
    property real slideProgress: 0
    Behavior on slideProgress {
        NumberAnimation {
            duration: 250
            easing.type: Easing.OutCubic
            onRunningChanged: {
                if (!running && !root.showing)
                    root.visible = false;
            }
        }
    }

    function showBar() {
        visible = true;
        showing = true;
        slideProgress = 1;
    }
    function hideBar() {
        showing = false;
        slideProgress = 0;
    }

    // ── Window configuration ──────────────────────────────────────────────────
    visible: false
    color: "transparent"
    WlrLayershell.layer: WlrLayer.Overlay
    WlrLayershell.namespace: "quickshell-system-status"
    anchors.bottom: true
    anchors.right: true
    readonly property int travelSize: barMargin + barHeight
    // Width covers the pill at max reasonable size (long SSID + all sections)
    // plus travelSize on the left to allow room for the diagonal slide-in.
    implicitWidth: 600 + travelSize
    implicitHeight: travelSize + barHeight + travelSize
    exclusiveZone: 0
    focusable: false
    mask: Region { item: pill }

    // ── Content ───────────────────────────────────────────────────────────────
    Item {
        anchors.fill: parent
        clip: true

        component Divider: Rectangle {
            anchors.verticalCenter: parent ? parent.verticalCenter : undefined
            width: root.dividerW
            height: root.dividerH
            color: Theme.grey0
            opacity: 0.5
        }

        Rectangle {
            id: pill
            height: root.barHeight
            radius: root.barRadius
            color: Theme.bg3
            clip: true

            width: barRow.implicitWidth + root.hPad * 2
            Behavior on width {
                NumberAnimation { duration: 200; easing.type: Easing.OutCubic }
            }

            // Pin right edge; pill grows leftward as width increases.
            anchors.right: parent.right
            anchors.rightMargin: root.barMargin
            anchors.bottom: parent.bottom
            anchors.bottomMargin: root.barMargin

            transform: Translate {
                // slide diagonally: right and down when hidden (bottom-right mirror of top-right)
                x: (1 - root.slideProgress) * root.travelSize
                y: (1 - root.slideProgress) * root.travelSize
            }

            // Visible content row
            Row {
                id: barRow
                anchors.verticalCenter: parent.verticalCenter
                x: root.hPad
                spacing: 0

                // ── Network ───────────────────────────────────────────────────
                Text {
                    anchors.verticalCenter: parent.verticalCenter
                    text: !root.networkConnected ? "󰤭" : root.networkIsWifi ? "󱚽" : "󰈀"
                    color: root.networkConnected ? Theme.foreground : Theme.grey0
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: root.fontSize
                    verticalAlignment: Text.AlignVCenter
                }

                Item { width: root.itemSpacing; height: 1 }

                Text {
                    anchors.verticalCenter: parent.verticalCenter
                    text: root.networkText
                    color: root.networkConnected ? Theme.foreground : Theme.grey0
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: root.fontSize
                    verticalAlignment: Text.AlignVCenter
                }

                Item { width: root.groupSpacing; height: 1 }
                Divider {}
                Item { width: root.groupSpacing; height: 1 }

                // ── VPN ───────────────────────────────────────────────────────
                Text {
                    anchors.verticalCenter: parent.verticalCenter
                    text: root.vpnConnected ? "󰌾" : "󰌿"
                    color: root.vpnConnected ? Theme.blue : Theme.orange
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: root.fontSize
                    verticalAlignment: Text.AlignVCenter
                }

                Item { width: root.groupSpacing; height: 1 }
                Divider {}
                Item { width: root.groupSpacing; height: 1 }

                // ── Battery ───────────────────────────────────────────────────
                Row {
                    anchors.verticalCenter: parent.verticalCenter
                    spacing: root.itemSpacing
                    visible: root.hasBattery

                    Text {
                        anchors.verticalCenter: parent.verticalCenter
                        text: root.batteryIcon(root.batteryText, root.batteryCharging)
                        color: root.batteryColor(root.batteryText, root.batteryCharging)
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: root.fontSize
                        verticalAlignment: Text.AlignVCenter
                    }

                    Text {
                        anchors.verticalCenter: parent.verticalCenter
                        text: root.batteryText
                        color: root.batteryColor(root.batteryText, root.batteryCharging)
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: root.fontSize
                        verticalAlignment: Text.AlignVCenter
                    }
                }

                Item {
                    width: root.hasBattery ? root.groupSpacing : 0
                    height: 1
                }
                Divider {
                    visible: root.hasBattery
                }
                Item {
                    width: root.hasBattery ? root.groupSpacing : 0
                    height: 1
                }

                // ── Power profile ─────────────────────────────────────────────
                Text {
                    anchors.verticalCenter: parent.verticalCenter
                    text: root.powerIcon(root.powerProfile)
                    color: {
                        if (root.powerProfile === "performance") return Theme.yellow;
                        if (root.powerProfile === "power-saver") return Theme.aqua;
                        return Theme.foreground;
                    }
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: root.fontSize
                    verticalAlignment: Text.AlignVCenter
                }

                Item { width: root.groupSpacing; height: 1 }
                Divider {}
                Item { width: root.groupSpacing; height: 1 }

                // ── Systemd ───────────────────────────────────────────────────
                Text {
                    anchors.verticalCenter: parent.verticalCenter
                    text: root.systemdOk ? "󰄳" : ("󰀩 " + root.systemdFailed)
                    color: root.systemdOk ? Theme.green : Theme.red
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: root.fontSize
                    verticalAlignment: Text.AlignVCenter
                }
            }
        }
    }
}
