import Quickshell
import Quickshell.Hyprland
import QtQml

// Entry point for quickshell widgets.
//
// Each widget is a separate component file in this directory.
// To add a new widget:
//   1. Create a new QML file (e.g. MyWidget.qml) with a PanelWindow as root
//   2. Add it as a child of ShellRoot below
//   3. Wire up any Hyprland events in the Connections block if needed
//
// All theme colors are available via the Theme singleton, e.g.:
//   color: Theme.red
//   color: Theme.bg0
//
// See Theme.qml for the full list of available colors.

ShellRoot {
    SubMapOverlay {
        id: submapOverlay
    }

    WorkspaceBar {
        id: workspaceBar
    }

    NowPlaying {
        id: nowPlaying
    }

    Environment {
        id: environment
    }

    WindowTitle {
        id: windowTitle
    }

    StatusBar {
        id: statusBar
    }

    OsdOverlay {
        id: osd
    }

    // Hyprland IPC event handler
    // Add event routing for new widgets here
    Connections {
        target: Hyprland
        function onRawEvent(event) {
            // submap events
            if (event.name === "submap") {
                var submapName = event.parse(1)[0] ?? "";
                if (submapName === "") {
                    submapOverlay.hideOverlay();
                } else {
                    submapOverlay.showOverlay(submapName);
                }
            }

            // custom events for quickshell widget visibility
            if (event.name === "custom") {
                var data = event.parse(1)[0] ?? "";
                if (data === "quickshell:show") {
                    workspaceBar.showBar();
                    nowPlaying.showBar();
                    windowTitle.showBar();
                    environment.showBar();
                    statusBar.showBar();
                } else if (data === "quickshell:hide") {
                    workspaceBar.hideBar();
                    nowPlaying.hideBar();
                    windowTitle.hideBar();
                    environment.hideBar();
                    statusBar.hideBar();
                } else if (data === "quickshell:inhibit-on") {
                    statusBar.setInhibited(true);
                } else if (data === "quickshell:inhibit-off") {
                    statusBar.setInhibited(false);
                } else if (data.startsWith("quickshell:osd:")) {
                    // OSD events: quickshell:osd:<type>:<value>
                    // e.g. quickshell:osd:volume:75
                    //      quickshell:osd:volume:muted
                    //      quickshell:osd:brightness:40
                    //      quickshell:osd:touchpad:on
                    var parts = data.split(":");
                    if (parts.length >= 4) {
                        var osdType = parts[2];
                        var osdRaw = parts[3];
                        var osdValue;
                        if (osdRaw === "muted") {
                            osdValue = -1;
                        } else if (osdRaw === "on") {
                            osdValue = 1;
                        } else if (osdRaw === "off") {
                            osdValue = 0;
                        } else {
                            osdValue = parseInt(osdRaw, 10);
                        }
                        osd.show(osdType, osdValue);
                    }
                }
            }
        }
    }
}
