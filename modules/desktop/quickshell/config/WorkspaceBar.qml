import Quickshell
import Quickshell.Wayland
import Quickshell.Hyprland
import QtQuick

// Workspace bar indicator — minimal square-cell bar
//
// A vertical column of square workspace cells. Workspace 1 is at the top,
// workspace 9 at the bottom. The active workspace gets a gradient background
// that slides between cells with animation. Inactive cells use bg3.
// Slides in from the top-left corner on show.
//
// Visibility: hidden at rest, shown on Super hold via custom Hyprland
// IPC event (quickshell:show / quickshell:hide).
//
// Usage from shell.qml:
//   WorkspaceBar { id: workspaceBar }
//   workspaceBar.showBar()
//   workspaceBar.hideBar()

PanelWindow {
    id: barWindow

    // -- configuration --
    readonly property int workspaceCount: 9
    readonly property int cellWidth: 40
    readonly property int cellHeight: 40
    readonly property int barWidth: cellWidth
    readonly property int barHeight: cellHeight * workspaceCount
    readonly property int marginTop: 30
    readonly property int marginLeft: 30
    // rainbow cycles twice across the 9 workspaces
    readonly property real rainbowCycles: 2.0

    // -- state --
    property bool showing: false
    // shimmer offset — sweeps through the gradient on invocation
    property real gradientOffset: 0
    // single progress value (0 = hidden, 1 = shown) drives both slide axes
    property real slideProgress: 0
    Behavior on slideProgress {
        NumberAnimation {
            duration: 250
            easing.type: Easing.OutCubic
            onRunningChanged: {
                if (!running && !barWindow.showing) {
                    barWindow.visible = false;
                }
            }
        }
    }
    property int activeWs: {
        var fw = Hyprland.focusedWorkspace;
        if (fw && fw.id >= 1 && fw.id <= workspaceCount) return fw.id;
        return 1;
    }

    // animated highlight position (0-indexed, fractional during animation)
    property real highlightPos: activeWs - 1
    Behavior on highlightPos {
        NumberAnimation {
            duration: 200
            easing.type: Easing.OutBack
            easing.overshoot: 1.4
        }
    }

    // -- functions --
    function showBar() {
        visible = true;
        showing = true;
        slideProgress = 1;
    }
    function hideBar() {
        showing = false;
        slideProgress = 0;
    }

    // -- rainbow color helpers --
    // continuous shimmer — loops the gradient offset while the bar is visible
    NumberAnimation on gradientOffset {
        id: shimmerAnim
        from: 0; to: 1
        duration: 4000
        loops: Animation.Infinite
        running: barWindow.showing
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
            Theme.red, Theme.orange, Theme.yellow,
            Theme.green, Theme.aqua, Theme.blue, Theme.purple,
        ];
        var shifted = ((pos + gradientOffset) % 1.0 + 1.0) % 1.0;
        var scaled = shifted * colors.length;
        var i = Math.floor(scaled);
        var t = scaled - i;
        return lerpColor(colors[i % colors.length], colors[(i + 1) % colors.length], t);
    }

    // -- window configuration --
    visible: false
    color: "transparent"
    WlrLayershell.layer: WlrLayer.Overlay
    WlrLayershell.namespace: "quickshell-workspace-bar"
    anchors.top: true
    anchors.left: true
    // window must be large enough that the bar enters the clip region
    // at a diagonal — use the larger travel distance for both axes
    readonly property int travelSize: Math.max(marginLeft + barWidth, marginTop + barHeight)
    width: travelSize + barWidth
    height: travelSize + barHeight
    exclusiveZone: 0
    focusable: false

    // -- content --
    // clip so the bar is invisible while translated outside the window bounds
    Item {
        id: clipRoot
        anchors.fill: parent
        clip: true

        Item {
            id: contentRoot
            width: barWindow.barWidth
            height: barWindow.barHeight

            // resting position: at the margin offset
            // hidden position: translated diagonally so the bar sits just outside
            // the top-left corner, equidistant on both axes
            x: barWindow.marginLeft
            y: barWindow.marginTop

            transform: Translate {
                id: slideTranslate
                x: (1 - barWindow.slideProgress) * -barWindow.travelSize
                y: (1 - barWindow.slideProgress) * -barWindow.travelSize
            }

            // background bar
            Rectangle {
                anchors.fill: parent
                color: Theme.bg3
            }

            // sliding rainbow highlight
            Rectangle {
                id: highlight
                width: barWindow.cellWidth
                height: barWindow.cellHeight
                x: 0
                y: barWindow.highlightPos * barWindow.cellHeight

                // rainbow gradient with multiple stops for richer colour
                gradient: Gradient {
                    orientation: Gradient.Vertical
                    GradientStop {
                        position: 0.0
                        color: barWindow.rainbowAt(barWindow.highlightPos / barWindow.workspaceCount * barWindow.rainbowCycles)
                    }
                    GradientStop {
                        position: 0.25
                        color: barWindow.rainbowAt((barWindow.highlightPos + 0.25) / barWindow.workspaceCount * barWindow.rainbowCycles)
                    }
                    GradientStop {
                        position: 0.5
                        color: barWindow.rainbowAt((barWindow.highlightPos + 0.5) / barWindow.workspaceCount * barWindow.rainbowCycles)
                    }
                    GradientStop {
                        position: 0.75
                        color: barWindow.rainbowAt((barWindow.highlightPos + 0.75) / barWindow.workspaceCount * barWindow.rainbowCycles)
                    }
                    GradientStop {
                        position: 1.0
                        color: barWindow.rainbowAt((barWindow.highlightPos + 1) / barWindow.workspaceCount * barWindow.rainbowCycles)
                    }
                }
            }

            // workspace numbers
            Repeater {
                model: barWindow.workspaceCount

                Text {
                    required property int index
                    property bool isActive: (barWindow.activeWs - 1) === index
                    // find our workspace object from Hyprland so QML tracks changes
                    property var wsObject: {
                        var wsList = Hyprland.workspaces.values;
                        for (var i = 0; i < wsList.length; i++) {
                            if (wsList[i].id === index + 1) return wsList[i];
                        }
                        return null;
                    }
                    property bool occupied: wsObject ? wsObject.toplevels.values.length > 0 : false

                    // pre-compute the target color as a property so QML tracks
                    // all dependencies (activeWs, occupied, gradientOffset)
                    property color activeColor: Theme.bg0
                    property color occupiedColor: barWindow.rainbowAt(index / barWindow.workspaceCount * barWindow.rainbowCycles)
                    property color emptyColor: Theme.grey0
                    property color targetColor: isActive ? activeColor
                                              : occupied ? occupiedColor
                                              : emptyColor

                    x: 0
                    y: index * barWindow.cellHeight
                    width: barWindow.cellWidth
                    height: barWindow.cellHeight

                    text: (index + 1).toString()
                    color: targetColor
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: 18
                    font.weight: isActive ? Font.Bold : occupied ? Font.DemiBold : Font.Normal
                    horizontalAlignment: Text.AlignHCenter
                    verticalAlignment: Text.AlignVCenter
                }
            }
        }
    }
}
