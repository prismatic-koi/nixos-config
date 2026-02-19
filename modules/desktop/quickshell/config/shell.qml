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

    WorkspaceArc {
        id: workspaceArc
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
                    workspaceArc.showArc();
                } else if (data === "quickshell:hide") {
                    workspaceArc.hideArc();
                }
            }
        }
    }
}
