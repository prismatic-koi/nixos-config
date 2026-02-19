import Quickshell
import Quickshell.Wayland
import QtQuick
import QtQuick.Layouts

// Submap overlay widget
//
// Displays a floating pill at the top of the screen when a Hyprland submap
// is active, showing available keybinds. Slides down from above with a
// rotating rainbow gradient border.
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
    // Each entry is an array of { key, label } objects.
    property string currentSubmap: ""
    property var submapData: ({
        "exit": [
            { key: "l", label: "Lock" },
            { key: "s", label: "Shutdown" },
            { key: "r", label: "Reboot" },
            { key: "⇧L", label: "Logout" },
        ],
        "resize": [
            { key: "h", label: "←" },
            { key: "j", label: "↓" },
            { key: "k", label: "↑" },
            { key: "l", label: "→" },
        ],
    })

    // -- animation state --
    property bool showing: false

    function showOverlay(submapName) {
        currentSubmap = submapName;
        visible = true;
        showing = true;
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
    anchors.left: true
    anchors.right: true
    exclusiveZone: 0
    focusable: false
    implicitHeight: 90

    // -- content --
    Item {
        anchors.fill: parent
        anchors.topMargin: 10

        // gradient border (outer rectangle)
        Rectangle {
            id: borderRect
            anchors.horizontalCenter: parent.horizontalCenter
            width: contentRow.implicitWidth + 56
            height: 60
            radius: 14

            // slide + fade animation
            opacity: overlay.showing ? 0.95 : 0
            y: overlay.showing ? 0 : -70

            Behavior on opacity {
                NumberAnimation {
                    duration: 200
                    easing.type: Easing.OutCubic
                }
            }
            Behavior on y {
                NumberAnimation {
                    id: slideAnim
                    duration: 200
                    easing.type: Easing.OutCubic
                    onRunningChanged: {
                        if (!running && !overlay.showing) {
                            overlay.visible = false;
                            overlay.currentSubmap = "";
                        }
                    }
                }
            }

            // rotating rainbow gradient
            property real gradientOffset: 0
            NumberAnimation on gradientOffset {
                from: 0; to: 1
                duration: 3000
                loops: Animation.Infinite
                running: overlay.showing
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
                color: Theme.bgDim
            }

            // text content
            RowLayout {
                id: contentRow
                anchors.centerIn: parent
                spacing: 8

                // submap title
                Text {
                    text: overlay.currentSubmap.toUpperCase()
                    color: Theme.foreground
                    font.family: "JetBrains Mono"
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

                        Text {
                            text: modelData.label
                            color: Theme.foreground
                            font.family: "JetBrains Mono"
                            font.pixelSize: 20
                            font.weight: Font.DemiBold
                        }
                        Text {
                            text: "[" + modelData.key + "]"
                            color: Theme.grey1
                            font.family: "JetBrains Mono"
                            font.pixelSize: 17
                        }
                    }
                }
            }
        }
    }
}
