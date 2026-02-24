import Quickshell
import Quickshell.Wayland
import QtQuick
import QtQuick.Layouts

// OSD overlay widget
//
// Displays a GNOME-style floating card at 50% across, 75% down the screen
// when volume, brightness, or touchpad state changes. The card shows:
//   - A Nerd Font icon that reflects current state
//   - A label (e.g. "Volume", "Brightness", "Touchpad")
//   - A live percentage bar (for volume and brightness)
//   - A text percentage (or On/Off for touchpad)
//
// The card lingers for 1 second after the last change then fades out.
//
// Events are fired from shell scripts via:
//   hyprctl dispatch event quickshell:osd:volume:<0-150>
//   hyprctl dispatch event quickshell:osd:volume:muted
//   hyprctl dispatch event quickshell:osd:brightness:<0-100>
//   hyprctl dispatch event quickshell:osd:touchpad:on
//   hyprctl dispatch event quickshell:osd:touchpad:off
//
// Usage from shell.qml:
//   OsdOverlay { id: osd }
//   osd.show("volume", 75)
//   osd.show("volume", -1)     // -1 = muted
//   osd.show("brightness", 40)
//   osd.show("touchpad", 1)    // 1 = enabled, 0 = disabled

PanelWindow {
    id: osd

    // -- public API --
    function show(type, value) {
        osdType = type;
        osdValue = value;
        showing = true;
        lingerTimer.restart();
    }

    // -- internal state --
    property string osdType: "volume"   // "volume" | "brightness" | "touchpad"
    property int osdValue: 0            // 0-150 for volume, 0-100 for brightness, 1/0 for touchpad, -1 for muted
    property bool showing: false

    // -- linger timer: hide 1 second after the last event --
    Timer {
        id: lingerTimer
        interval: 1000
        repeat: false
        onTriggered: osd.showing = false
    }

    // -- derived display values --
    property string osdIcon: {
        if (osdType === "volume") {
            if (osdValue < 0) return "󰝟";           // muted
            if (osdValue === 0) return "󰕿";
            if (osdValue <= 33) return "󰖀";
            if (osdValue <= 66) return "󰕾";
            return "󰕾";                              // high (same glyph, bar shows level)
        }
        if (osdType === "brightness") {
            if (osdValue <= 50) return "󰃞";
            return "󰃟";
        }
        if (osdType === "touchpad") {
            return osdValue === 1 ? "󰟸" : "󰤳";
        }
        return "";
    }

    property string osdLabel: {
        if (osdType === "volume") return "Volume";
        if (osdType === "brightness") return "Brightness";
        if (osdType === "touchpad") return "Touchpad";
        return "";
    }

    property string osdValueText: {
        if (osdType === "touchpad") return osdValue === 1 ? "On" : "Off";
        if (osdValue < 0) return "Muted";
        return osdValue + "%";
    }

    // bar fill: 0.0–1.0 (touchpad and muted both render as 0)
    property real osdBarFill: {
        if (osdType === "touchpad" || osdValue < 0) return 0.0;
        if (osdType === "volume") return Math.min(osdValue / 100.0, 1.0);
        return Math.min(osdValue / 100.0, 1.0);
    }

    // bar tint color
    property color osdBarColor: {
        if (osdType === "volume") {
            if (osdValue < 0) return Theme.grey1;        // muted
            if (osdValue > 100) return Theme.orange;     // overamplified
            return Theme.blue;
        }
        if (osdType === "brightness") return Theme.yellow;
        if (osdType === "touchpad") return osdValue === 1 ? Theme.green : Theme.red;
        return Theme.blue;
    }

    // -- window configuration --
    // Full-screen transparent layer; card is positioned via anchors/margins
    visible: true
    color: "transparent"
    WlrLayershell.layer: WlrLayer.Overlay
    WlrLayershell.namespace: "quickshell-osd"
    anchors.top: true
    anchors.bottom: true
    anchors.left: true
    anchors.right: true
    exclusiveZone: 0
    focusable: false
    // Only the card itself intercepts input; the rest of the overlay is click-through
    mask: Region {
        item: card
    }

    // -- card --
    Item {
        anchors.fill: parent

        Rectangle {
            id: card
            width: 280
            height: 80
            radius: 16

            // positioned at 50% across, 75% down
            x: parent.width * 0.5 - width / 2
            y: parent.height * 0.75 - height / 2

            color: Theme.bg1
            border.color: Theme.bg3
            border.width: 1

            // entrance/exit animation: fade + subtle scale
            opacity: osd.showing ? 1.0 : 0.0
            scale: osd.showing ? 1.0 : 0.92

            Behavior on opacity {
                NumberAnimation {
                    duration: 150
                    easing.type: Easing.OutCubic
                }
            }
            Behavior on scale {
                NumberAnimation {
                    duration: 150
                    easing.type: Easing.OutCubic
                }
            }

            ColumnLayout {
                anchors {
                    left: parent.left
                    right: parent.right
                    verticalCenter: parent.verticalCenter
                    leftMargin: 20
                    rightMargin: 20
                }
                spacing: 8

                // -- top row: icon + label + value text --
                RowLayout {
                    Layout.fillWidth: true
                    spacing: 10

                    // state icon
                    Text {
                        text: osd.osdIcon
                        color: osd.osdBarColor
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: 22
                        Layout.alignment: Qt.AlignVCenter

                        Behavior on color {
                            ColorAnimation { duration: 120 }
                        }
                    }

                    // label
                    Text {
                        text: osd.osdLabel
                        color: Theme.foreground
                        font.family: "Noto Sans"
                        font.pixelSize: 15
                        font.weight: Font.DemiBold
                        Layout.fillWidth: true
                        Layout.alignment: Qt.AlignVCenter
                    }

                    // value
                    Text {
                        text: osd.osdValueText
                        color: Theme.secondary
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: 14
                        Layout.alignment: Qt.AlignVCenter

                        Behavior on text {
                            // no animation needed — just snaps
                        }
                    }
                }

                // -- progress bar (hidden for touchpad) --
                Rectangle {
                    Layout.fillWidth: true
                    height: 6
                    radius: 3
                    color: Theme.bg3
                    visible: osd.osdType !== "touchpad"

                    // filled portion
                    Rectangle {
                        width: parent.width * osd.osdBarFill
                        height: parent.height
                        radius: parent.radius
                        color: osd.osdBarColor

                        Behavior on width {
                            NumberAnimation {
                                duration: 100
                                easing.type: Easing.OutCubic
                            }
                        }
                        Behavior on color {
                            ColorAnimation { duration: 120 }
                        }
                    }
                }

                // -- touchpad indicator (shown instead of bar) --
                Rectangle {
                    Layout.fillWidth: true
                    height: 6
                    radius: 3
                    color: osd.osdBarColor
                    visible: osd.osdType === "touchpad"
                    opacity: 0.35

                    Behavior on color {
                        ColorAnimation { duration: 120 }
                    }
                }
            }
        }
    }
}
