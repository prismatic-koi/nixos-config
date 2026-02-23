import Quickshell
import Quickshell.Wayland
import QtQuick
import QtQuick.Layouts

// Submap overlay widget
//
// Displays a floating pill at the centre of the screen when a Hyprland submap
// is active, showing available keybinds. Expands from centre with a
// rainbow gradient border that shimmers on invocation.
//
// Design language compliance:
//   - Position: centre of screen, expands from centre
//   - Typography: three-level hierarchy
//       Mode label: Noto Sans bold, text/primary or semantic colour
//       Action labels: JetBrains Mono DemiBold, semantic colours
//       Shortcut hints: JetBrains Mono regular, muted
//   - Opacity: 1.0 (fully opaque invoked surface)
//   - Background: bg0 (background role)
//   - Border: rainbow gradient with brief shimmer on invocation
//   - Animation: spring physics (overshoot and settle)
//   - Dimming: per-submap scrim for high-stakes submaps (exit)
//
// Usage from shell.qml:
//   SubMapOverlay { id: submapOverlay }
//   submapOverlay.showOverlay("exit")
//   submapOverlay.hideOverlay()
//
// To add a new submap, add an entry to the submapData property below.

PanelWindow {
    id: overlay

    // -- submap definitions --
    // Add new submaps here as entries in this object.
    // Each entry is an array of { key, label, color? } objects.
    // If color is omitted, Theme.foreground is used.
    property string currentSubmap: ""
    property var submapData: ({
        "exit": [
            { key: "l", label: "Lock", color: Theme.grey2 },
            { key: "s", label: "Shutdown", color: Theme.orange },
            { key: "r", label: "Reboot", color: Theme.yellow },
            { key: "⇧L", label: "Logout", color: Theme.foreground },
        ],
        "resize": [
            { key: "h", label: "\u2190" },
            { key: "j", label: "\u2193" },
            { key: "k", label: "\u2191" },
            { key: "l", label: "\u2192" },
        ],
    })

    // semantic title colours per submap (default: Theme.foreground)
    property var submapTitleColors: ({
        "exit": Theme.red,
    })

    // submaps that dim the background (high-stakes / pause interaction)
    property var submapDims: ({
        "exit": true,
    })

    // -- animation state --
    property bool showing: false
    property bool shouldDim: false

    function showOverlay(submapName) {
        currentSubmap = submapName;
        shouldDim = submapDims[submapName] ?? false;
        visible = true;
        showing = true;
        shimmerAnim.restart();
    }
    function hideOverlay() {
        showing = false;
    }

    // -- window configuration --
    visible: false
    color: "transparent"
    WlrLayershell.layer: WlrLayer.Overlay
    WlrLayershell.namespace: "quickshell-submap"
    anchors.top: true
    anchors.bottom: true
    anchors.left: true
    anchors.right: true
    exclusiveZone: 0
    focusable: false

    // -- content --
    Item {
        anchors.fill: parent

        // scrim — dims the background for high-stakes submaps
        Rectangle {
            id: scrim
            anchors.fill: parent
            color: Theme.bgDim
            opacity: (overlay.showing && overlay.shouldDim) ? 0.45 : 0

            Behavior on opacity {
                NumberAnimation {
                    duration: 200
                    easing.type: Easing.OutCubic
                }
            }
        }

        // gradient border (outer rectangle)
        Rectangle {
            id: borderRect
            anchors.centerIn: parent
            width: contentRow.implicitWidth + 56
            height: 60
            radius: 14

            // expand from centre + fade
            opacity: overlay.showing ? 1.0 : 0
            scale: overlay.showing ? 1.0 : 0.8

            Behavior on opacity {
                NumberAnimation {
                    duration: 200
                    easing.type: Easing.OutBack
                    easing.overshoot: 1.2
                }
            }
            Behavior on scale {
                NumberAnimation {
                    id: scaleAnim
                    duration: 200
                    easing.type: Easing.OutBack
                    easing.overshoot: 1.4
                    onRunningChanged: {
                        if (!running && !overlay.showing) {
                            overlay.visible = false;
                            overlay.currentSubmap = "";
                            overlay.shouldDim = false;
                        }
                    }
                }
            }

            // rainbow gradient with invocation shimmer
            property real gradientOffset: 0

            // brief shimmer on invocation — sweeps once then stops
            NumberAnimation {
                id: shimmerAnim
                target: borderRect
                property: "gradientOffset"
                from: 0; to: 1
                duration: 800
                easing.type: Easing.InOutQuad
            }

            function lerpColor(c1, c2, t) {
                return Qt.rgba(
                    c1.r + (c2.r - c1.r) * t,
                    c1.g + (c2.g - c1.g) * t,
                    c1.b + (c2.b - c1.b) * t,
                    1
                );
            }
            function rainbowAt(pos) {
                var colors = [
                    Theme.red,
                    Theme.orange,
                    Theme.yellow,
                    Theme.green,
                    Theme.aqua,
                    Theme.blue,
                    Theme.purple,
                ];
                var shifted = (pos + gradientOffset) % 1.0;
                var scaled = shifted * colors.length;
                var i = Math.floor(scaled);
                var t = scaled - i;
                return lerpColor(colors[i % colors.length], colors[(i + 1) % colors.length], t);
            }

            gradient: Gradient {
                orientation: Gradient.Horizontal
                GradientStop { position: 0.0; color: borderRect.rainbowAt(0.0) }
                GradientStop { position: 0.14; color: borderRect.rainbowAt(0.14) }
                GradientStop { position: 0.28; color: borderRect.rainbowAt(0.28) }
                GradientStop { position: 0.42; color: borderRect.rainbowAt(0.42) }
                GradientStop { position: 0.57; color: borderRect.rainbowAt(0.57) }
                GradientStop { position: 0.71; color: borderRect.rainbowAt(0.71) }
                GradientStop { position: 0.85; color: borderRect.rainbowAt(0.85) }
                GradientStop { position: 1.0; color: borderRect.rainbowAt(1.0) }
            }

            // inner fill (covers the gradient except the 2px border)
            Rectangle {
                anchors.fill: parent
                anchors.margins: 2
                radius: 12
                color: Theme.bg0
            }

            // text content
            RowLayout {
                id: contentRow
                anchors.centerIn: parent
                spacing: 8

                // mode label — Noto Sans bold (sans names contexts)
                Text {
                    text: overlay.currentSubmap.toUpperCase()
                    color: overlay.submapTitleColors[overlay.currentSubmap] ?? Theme.foreground
                    font.family: "Noto Sans"
                    font.pixelSize: 22
                    font.weight: Font.Bold
                    Layout.rightMargin: 10
                }

                // separator
                Rectangle {
                    width: 2
                    height: 28
                    Layout.alignment: Qt.AlignVCenter
                    color: Theme.grey0
                }

                // keybind options
                Repeater {
                    model: overlay.submapData[overlay.currentSubmap] ?? []

                    delegate: RowLayout {
                        required property var modelData
                        spacing: 4
                        Layout.leftMargin: 8

                        // action label — JetBrains Mono DemiBold, semantic colour
                        Text {
                            text: modelData.label
                            color: modelData.color ?? Theme.foreground
                            font.family: "JetBrainsMono Nerd Font"
                            font.pixelSize: 20
                            font.weight: Font.DemiBold
                        }
                        // shortcut hint — JetBrains Mono regular, muted
                        Text {
                            text: "[" + modelData.key + "]"
                            color: Theme.grey1
                            font.family: "JetBrainsMono Nerd Font"
                            font.pixelSize: 17
                        }
                    }
                }
            }
        }
    }
}
