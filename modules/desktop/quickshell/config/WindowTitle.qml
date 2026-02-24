import Quickshell
import Quickshell.Wayland
import Quickshell.Hyprland
import QtQuick

// Window title indicator — top-centre pill showing the focused window title.
//
// A rounded-rectangle pill centred at the top of the screen that displays the
// active window's title. It slides in from above and smoothly widens/narrows
// as the title changes between windows.
//
// Visibility: hidden at rest, shown on Super hold via custom Hyprland IPC
// event (quickshell:show / quickshell:hide). Hidden when no window is focused.
//
// Usage from shell.qml:
//   WindowTitle { id: windowTitle }
//   windowTitle.showBar()
//   windowTitle.hideBar()

PanelWindow {
    id: titleWindow

    // -- configuration --
    readonly property int marginTop: 30
    readonly property int pillHeight: 40
    readonly property int pillRadius: 5
    readonly property int pillPaddingH: 16 // horizontal padding inside pill
    readonly property int maxTextWidth: 800
    // travel distance: pill must move far enough to clear the top edge
    readonly property int travelSize: marginTop + pillHeight

    // -- state --
    property bool showing: false
    property real slideProgress: 0

    Behavior on slideProgress {
        NumberAnimation {
            duration: 250
            easing.type: Easing.OutCubic
            onRunningChanged: {
                if (!running && !titleWindow.showing) {
                    titleWindow.visible = false;
                }
            }
        }
    }

    // -- active window title (with browser-suffix rewrites) --
    // Only show a title when the focused workspace actually has windows.
    // Hyprland.activeToplevel persists across workspace switches so we
    // guard against empty workspaces by checking the toplevel count.
    property bool workspaceHasWindows: {
        var fw = Hyprland.focusedWorkspace;
        return fw ? fw.toplevels.values.length > 0 : false;
    }
    property string rawTitle: (workspaceHasWindows && Hyprland.activeToplevel) ? Hyprland.activeToplevel.title : ""
    property string windowTitle: {
        var t = rawTitle;
        // strip " — Mozilla Firefox" suffix
        t = t.replace(/ — Mozilla Firefox$/, "");
        // strip " - qutebrowser" suffix
        t = t.replace(/ - qutebrowser$/, "");
        return t;
    }

    // -- functions --
    function showBar() {
        if (!windowTitle)
            return; // nothing to show when no window is focused
        visible = true;
        showing = true;
        slideProgress = 1;
    }
    function hideBar() {
        showing = false;
        slideProgress = 0;
    }

    // auto-hide if the focused window goes away while Super is held
    onWindowTitleChanged: {
        if (!windowTitle && showing) {
            showing = false;
            slideProgress = 0;
        }
    }

    // -- window configuration --
    // full-width top-anchored overlay; content is centred inside
    visible: false
    color: "transparent"
    WlrLayershell.layer: WlrLayer.Overlay
    WlrLayershell.namespace: "quickshell-window-title"
    anchors.top: true
    anchors.left: true
    anchors.right: true
    implicitWidth: 1 // overridden by Wayland when left+right anchors are both set
    implicitHeight: travelSize + pillHeight
    exclusiveZone: 0
    focusable: false
    mask: Region {
        item: contentRoot
    }

    // -- content --
    // clip so the pill is invisible while translated above the window bounds
    Item {
        id: clipRoot
        anchors.fill: parent
        clip: true

        Item {
            id: contentRoot
            // match the pill dimensions so the input mask is correct
            width: pill.width
            height: titleWindow.pillHeight

            // resting position: centred horizontally, at marginTop
            x: (parent.width - width) / 2
            y: titleWindow.marginTop

            // slide straight up when hidden — same pattern as WorkspaceBar/NowPlaying
            transform: Translate {
                y: (1 - titleWindow.slideProgress) * -titleWindow.travelSize
            }

            // pill background — width animates as the title changes
            Rectangle {
                id: pill
                anchors.centerIn: parent
                height: titleWindow.pillHeight
                radius: titleWindow.pillRadius
                color: Theme.bg3

                property int targetWidth: Math.min(
                    titleText.implicitWidth + titleWindow.pillPaddingH * 2,
                    titleWindow.maxTextWidth + titleWindow.pillPaddingH * 2
                )
                width: targetWidth

                Behavior on width {
                    NumberAnimation {
                        duration: 200
                        easing.type: Easing.OutCubic
                    }
                }

                Text {
                    id: titleText
                    anchors.centerIn: parent
                    width: Math.min(implicitWidth, titleWindow.maxTextWidth)
                    text: titleWindow.windowTitle
                    color: Theme.foreground
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: 18
                    font.weight: Font.DemiBold
                    elide: Text.ElideRight
                    maximumLineCount: 1
                    horizontalAlignment: Text.AlignHCenter
                }
            }
        }
    }
}
