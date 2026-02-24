import Quickshell
import Quickshell.Wayland
import Quickshell.Io
import QtQuick

// Status bar — top-right pill with status indicators and datetime
//
// Layout (left → right inside a single pill):
//
//   [ syncthing ] | [ audio ] | [ notifications ] | [ idle-inhibit ] | [ date  time ]
//
// The idle-inhibit section collapses to zero width when inactive, and the pill
// width animates smoothly between the two stable states.

PanelWindow {
    id: root

    // -- configuration --
    readonly property int barHeight: 40
    readonly property int barMargin: 30
    readonly property int barRadius: 5
    readonly property int hPad: 14        // horizontal padding inside pill
    readonly property int fontSize: 18    // uniform font size for all elements
    readonly property int itemSpacing: 14 // gap between items within a group
    readonly property int dividerW: 1     // divider width
    readonly property int dividerH: 20   // divider height (shorter than pill)
    readonly property int groupSpacing: 14 // gap between divider and neighbouring items
    readonly property bool hasExternalAudio: Theme.externalAudio

    // -- state: syncthing --
    property bool syncthingOk: true

    // -- state: audio (external DAC mode) --
    property bool dacConnected: true

    // -- state: audio (internal mode) --
    property int volume: 0
    property bool muted: false

    // -- state: notifications --
    property bool hasNotifications: false

    // -- state: idle inhibit --
    property bool idleInhibited: false

    // -- state: datetime --
    property string dateStr: ""
    property string timeStr: ""

    // ── Syncthing polling (every 10 s) ────────────────────────────────────────
    Process {
        id: syncProc
        command: ["sh", "-c", "syncthing cli show system 2>/dev/null && echo OK || echo FAIL"]
        stdout: StdioCollector {
            onStreamFinished: {
                var out = this.text.trim();
                if (out === "FAIL" || out.length === 0) {
                    root.syncthingOk = false;
                    return;
                }
                root.syncthingOk = true;
            }
        }
    }

    Process {
        id: syncFolderProc
        command: ["sh", "-c", "syncthing cli show folder-errors 2>/dev/null | jq -e 'to_entries | map(.value | length > 0) | any' 2>/dev/null && echo HAS_ERRORS || echo OK"]
        stdout: StdioCollector {
            onStreamFinished: {
                var out = this.text.trim();
                if (out === "HAS_ERRORS")
                    root.syncthingOk = false;
            }
        }
    }

    Timer {
        interval: 10000
        repeat: true
        running: true
        onTriggered: {
            syncProc.running = true;
            syncFolderProc.running = true;
        }
    }

    Timer {
        interval: 200
        repeat: false
        running: true
        onTriggered: {
            syncProc.running = true;
            syncFolderProc.running = true;
        }
    }

    // ── Audio polling (every 2 s) ─────────────────────────────────────────────
    Process {
        id: audioProc
        command: {
            if (root.hasExternalAudio) {
                return ["sh", "-c", "wpctl inspect @DEFAULT_SINK@ 2>/dev/null | grep -m1 'node.nick' | awk -F'\"' '{print $2}'"];
            } else {
                return ["sh", "-c", "wpctl get-volume @DEFAULT_SINK@ 2>/dev/null"];
            }
        }
        stdout: StdioCollector {
            onStreamFinished: {
                var out = this.text.trim();
                if (root.hasExternalAudio) {
                    root.dacConnected = (out === "FiiO K7");
                } else {
                    root.muted = out.indexOf("[MUTED]") >= 0;
                    var match = out.match(/Volume:\s*([\d.]+)/);
                    if (match)
                        root.volume = Math.round(parseFloat(match[1]) * 100);
                }
            }
        }
    }

    Timer {
        interval: 2000
        repeat: true
        running: true
        onTriggered: audioProc.running = true
    }

    Timer {
        interval: 300
        repeat: false
        running: true
        onTriggered: audioProc.running = true
    }

    // ── Notification polling (every 2 s via swaync-client) ────────────────────
    // Use -c (count) not -swb — -swb is a blocking subscriber stream that never
    // finishes, so StdioCollector.onStreamFinished never fires.
    Process {
        id: notifProc
        command: ["sh", "-c", "swaync-client -c 2>/dev/null"]
        stdout: StdioCollector {
            onStreamFinished: {
                var count = parseInt(this.text.trim(), 10);
                root.hasNotifications = !isNaN(count) && count > 0;
            }
        }
    }

    Timer {
        interval: 2000
        repeat: true
        running: true
        onTriggered: notifProc.running = true
    }

    Timer {
        interval: 400
        repeat: false
        running: true
        onTriggered: notifProc.running = true
    }

    // ── Idle inhibit polling (every 2 s) ──────────────────────────────────────
    Process {
        id: inhibitProc
        command: ["sh", "-c", "LOCKFILE=\"${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/systemd_inhibit.lock\"; [ -f \"$LOCKFILE\" ] && pid=$(cat \"$LOCKFILE\" 2>/dev/null) && kill -0 \"$pid\" 2>/dev/null && echo active || echo inactive"]
        stdout: StdioCollector {
            onStreamFinished: {
                root.idleInhibited = (this.text.trim() === "active");
            }
        }
    }

    Timer {
        interval: 2000
        repeat: true
        running: true
        onTriggered: inhibitProc.running = true
    }

    Timer {
        interval: 500
        repeat: false
        running: true
        onTriggered: inhibitProc.running = true
    }

    // ── Clock (every 1 s) ─────────────────────────────────────────────────────
    Timer {
        interval: 1000
        repeat: true
        running: true
        triggeredOnStart: true
        onTriggered: {
            var now = new Date();
            var y = now.getFullYear();
            var m = String(now.getMonth() + 1).padStart(2, "0");
            var d = String(now.getDate()).padStart(2, "0");
            root.dateStr = y + "-" + m + "-" + d;
            var hh = String(now.getHours()).padStart(2, "0");
            var mm = String(now.getMinutes()).padStart(2, "0");
            var ss = String(now.getSeconds()).padStart(2, "0");
            root.timeStr = hh + ":" + mm + ":" + ss;
        }
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
    function setInhibited(active) {
        idleInhibited = active;
    }

    // ── Window configuration ──────────────────────────────────────────────────
    visible: false
    color: "transparent"
    WlrLayershell.layer: WlrLayer.Overlay
    WlrLayershell.namespace: "quickshell-status-bar"
    anchors.top: true
    anchors.right: true
    readonly property int travelSize: barMargin + barHeight
    // Fixed window size — large enough to hold the pill at maximum width.
    // Extra travelSize on left and bottom gives room for the diagonal slide.
    // The window does NOT resize during animation; only the pill moves within it.
    implicitWidth: 1200 + travelSize
    implicitHeight: travelSize + barHeight + travelSize
    exclusiveZone: 0
    focusable: false
    mask: Region { item: pill }

    // ── Content ───────────────────────────────────────────────────────────────
    Item {
        anchors.fill: parent
        clip: true

        // Helper component: a thin vertical divider between groups
        component Divider: Rectangle {
            anchors.verticalCenter: parent ? parent.verticalCenter : undefined
            width: root.dividerW
            height: root.dividerH
            color: Theme.grey0
            opacity: 0.5
        }

        // Off-screen measurement items — invisible, used only to pre-compute widths
        // so pill.width can jump between two known stable values without jitter.
        Text {
            id: inhibitMeasure
            visible: false
            text: "IDLE INHIBIT"
            font.family: "JetBrainsMono Nerd Font"
            font.pixelSize: root.fontSize
            font.weight: Font.Bold
        }

        Text {
            id: volMeasure
            visible: false
            text: "100%"
            font.family: "JetBrainsMono Nerd Font"
            font.pixelSize: root.fontSize
        }

        // The inhibit section's full width: spacer | badge | spacer | divider
        // The divider on the LEFT (after notif) and spacer on the RIGHT (before datetime)
        // are always present in barRow, so they stay visible when inactive.
        readonly property int inhibitSectionWidth: root.groupSpacing
                                                   + (inhibitMeasure.implicitWidth + 14)
                                                   + root.groupSpacing + root.dividerW

        Rectangle {
            id: pill
            height: root.barHeight
            radius: root.barRadius
            color: Theme.bg3
            clip: true

            // Base width: syncthing | divider | audio | divider | notif | divider | date time
            // The inhibit section is added on top when active.
            readonly property int baseWidth: baseRow.implicitWidth + root.hPad * 2
            width: root.idleInhibited
                   ? baseWidth + parent.inhibitSectionWidth
                   : baseWidth
            Behavior on width {
                NumberAnimation { duration: 200; easing.type: Easing.OutCubic }
            }

            // Pin right edge; pill grows leftward as width increases.
            anchors.right: parent.right
            anchors.rightMargin: root.barMargin
            y: root.barMargin

            transform: Translate {
                // slide diagonally: right and up when hidden (mirrors NowPlaying's bottom-left)
                x: (1 - root.slideProgress) * root.travelSize
                y: (1 - root.slideProgress) * -root.travelSize
            }

            // baseRow: invisible measurement row containing everything EXCEPT
            // the inhibit badge section. Gives us a stable implicitWidth.
            Row {
                id: baseRow
                visible: false
                spacing: 0

                Text {
                    text: "󰓦"
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: root.fontSize
                }
                Item { width: root.groupSpacing; height: 1 }
                Rectangle { width: root.dividerW; height: root.dividerH }
                Item { width: root.groupSpacing; height: 1 }

                Text {
                    text: root.hasExternalAudio ? "󰤽" : "󰕾"
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: root.fontSize
                }
                Item {
                    width: root.hasExternalAudio ? 0 : (volMeasure.implicitWidth + root.itemSpacing)
                    height: 1
                }
                Item { width: root.groupSpacing; height: 1 }
                Rectangle { width: root.dividerW; height: root.dividerH }
                Item { width: root.groupSpacing; height: 1 }

                Text {
                    text: "󰂚"
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: root.fontSize
                }
                Item { width: root.groupSpacing; height: 1 }
                Rectangle { width: root.dividerW; height: root.dividerH }
                Item { width: root.groupSpacing; height: 1 }

                Text {
                    text: "2026-02-24"
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: root.fontSize
                }
                Item { width: root.itemSpacing; height: 1 }
                Text {
                    text: "00:00:00"
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: root.fontSize
                    font.weight: Font.DemiBold
                }
            }

            // Visible content row
            Row {
                id: barRow
                anchors.verticalCenter: parent.verticalCenter
                x: root.hPad
                spacing: 0

                // ── Syncthing ─────────────────────────────────────────────────
                Text {
                    anchors.verticalCenter: parent.verticalCenter
                    text: root.syncthingOk ? "󰓦" : "󰗼"
                    color: root.syncthingOk ? Theme.green : Theme.red
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: root.fontSize
                    verticalAlignment: Text.AlignVCenter
                }

                Item { width: root.groupSpacing; height: 1 }
                Divider {}
                Item { width: root.groupSpacing; height: 1 }

                // ── Audio ─────────────────────────────────────────────────────
                Row {
                    visible: root.hasExternalAudio
                    spacing: 0
                    anchors.verticalCenter: parent.verticalCenter

                    Text {
                        anchors.verticalCenter: parent.verticalCenter
                        text: "󰤽"
                        color: root.dacConnected ? Theme.foreground : Theme.orange
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: root.fontSize
                        verticalAlignment: Text.AlignVCenter
                    }
                }

                Row {
                    visible: !root.hasExternalAudio
                    spacing: root.itemSpacing
                    anchors.verticalCenter: parent.verticalCenter

                    Text {
                        anchors.verticalCenter: parent.verticalCenter
                        text: root.muted ? "󰖁" : "󰕾"
                        color: root.muted ? Theme.grey1 : Theme.foreground
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: root.fontSize
                        verticalAlignment: Text.AlignVCenter
                    }

                    Text {
                        anchors.verticalCenter: parent.verticalCenter
                        text: root.volume + "%"
                        color: root.muted ? Theme.grey1 : Theme.foreground
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: root.fontSize
                        verticalAlignment: Text.AlignVCenter
                    }
                }

                Item { width: root.groupSpacing; height: 1 }
                Divider {}
                Item { width: root.groupSpacing; height: 1 }

                // ── Notifications ─────────────────────────────────────────────
                Text {
                    anchors.verticalCenter: parent.verticalCenter
                    text: root.hasNotifications ? "󰂚" : "󰂜"
                    color: root.hasNotifications ? Theme.red : Theme.grey0
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: root.fontSize
                    verticalAlignment: Text.AlignVCenter
                }

                // ── Idle inhibit badge ────────────────────────────────────────
                // The left divider (after notif) is always present here in barRow.
                // The inhibit section only collapses the badge + its right-side divider.

                Item { width: root.groupSpacing; height: 1 }
                Divider {}

                Item {
                    id: inhibitSection
                    anchors.verticalCenter: parent.verticalCenter
                    height: parent.height
                    clip: true
                    width: root.idleInhibited ? pill.parent.inhibitSectionWidth : 0
                    Behavior on width {
                        NumberAnimation { duration: 200; easing.type: Easing.OutCubic }
                    }

                    // Inner row pinned to the right edge so the badge slides in from right.
                    Row {
                        anchors.right: parent.right
                        anchors.verticalCenter: parent.verticalCenter
                        spacing: 0

                        Item { width: root.groupSpacing; height: 1 }

                        Rectangle {
                            anchors.verticalCenter: parent.verticalCenter
                            height: 26
                            width: inhibitMeasure.implicitWidth + 14
                            radius: 4
                            color: Theme.red

                            Text {
                                anchors.centerIn: parent
                                text: "IDLE INHIBIT"
                                color: Theme.bg0
                                font.family: "JetBrainsMono Nerd Font"
                                font.pixelSize: root.fontSize
                                font.weight: Font.Bold
                                verticalAlignment: Text.AlignVCenter
                            }
                        }

                        Item { width: root.groupSpacing; height: 1 }
                        Divider {}
                    }
                }

                Item { width: root.groupSpacing; height: 1 }

                // ── DateTime ──────────────────────────────────────────────────
                Row {
                    anchors.verticalCenter: parent.verticalCenter
                    spacing: root.itemSpacing

                    Text {
                        anchors.verticalCenter: parent.verticalCenter
                        text: root.dateStr
                        color: Theme.grey2
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: root.fontSize
                        verticalAlignment: Text.AlignVCenter
                    }

                    Text {
                        anchors.verticalCenter: parent.verticalCenter
                        text: root.timeStr
                        color: Theme.foreground
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: root.fontSize
                        font.weight: Font.DemiBold
                        verticalAlignment: Text.AlignVCenter
                    }
                }
            }
        }
    }
}
