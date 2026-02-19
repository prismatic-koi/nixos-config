import Quickshell
import Quickshell.Wayland
import Quickshell.Hyprland
import QtQuick

// Workspace arc indicator — single brushstroke
//
// A thick continuous arc sweeping from 7 o'clock up and over the top
// to 2 o'clock (~150° of arc). One organic brushstroke, no gaps or
// segments. The active workspace is a rainbow-gradient region that
// slides along the arc; the rest is bg3. Workspace numbers sit along
// the arc at evenly spaced positions.
//
// Visibility: hidden at rest, shown on Super hold via custom Hyprland
// IPC event (quickshell:show / quickshell:hide).
//
// Usage from shell.qml:
//   WorkspaceArc { id: workspaceArc }
//   workspaceArc.showArc()
//   workspaceArc.hideArc()

PanelWindow {
    id: arcWindow

    // -- configuration --
    readonly property int workspaceCount: 5
    readonly property real arcRadius: 125          // centreline radius
    readonly property real arcThickness: 120       // massively oversized — mask clips to shape
    readonly property int windowSize: 340

    // Segment positions: workspaces are placed along 7 o'clock → 2 o'clock
    // (canvas 120° → 330°, 210° clockwise sweep).
    // The colour fill goes a full 360° so the brushstroke mask is
    // completely filled everywhere — no gaps.
    readonly property real segStartDeg: 105        // ~8 o'clock — where workspace 1 starts
    readonly property real segSweepDeg: 195        // workspace region sweep

    // convert segment parameter t (0..1) to canvas degrees within the segment range
    function segAngle(t) {
        return segStartDeg + t * segSweepDeg;
    }

    // -- state --
    property bool showing: false
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
    function showArc() {
        visible = true;
        showing = true;
    }
    function hideArc() {
        showing = false;
    }

    // t value for workspace centre (0-indexed)
    function wsT(i) {
        var segSize = 1.0 / workspaceCount;
        return segSize * (i + 0.5);
    }

    // angle helpers
    function deg2rad(d) { return d * Math.PI / 180; }
    function px(r, deg) { return windowSize / 2 + r * Math.cos(deg2rad(deg)); }
    function py(r, deg) { return windowSize / 2 + r * Math.sin(deg2rad(deg)); }

    // -- window configuration --
    visible: false
    color: "transparent"
    WlrLayershell.layer: WlrLayer.Overlay
    WlrLayershell.namespace: "quickshell-workspace-arc"
    anchors.top: true
    anchors.left: true
    width: windowSize
    height: windowSize
    margins.top: 8
    margins.left: 8
    exclusiveZone: 0
    focusable: false

    // -- content --
    Item {
        id: contentRoot
        anchors.fill: parent

        opacity: arcWindow.showing ? 0.92 : 0
        scale: arcWindow.showing ? 1.0 : 0.80

        Behavior on opacity {
            NumberAnimation {
                duration: 200
                easing.type: Easing.OutCubic
                onRunningChanged: {
                    if (!running && !arcWindow.showing) {
                        arcWindow.visible = false;
                    }
                }
            }
        }
        Behavior on scale {
            NumberAnimation {
                duration: 250
                easing.type: Easing.OutBack
                easing.overshoot: 1.3
            }
        }

        Canvas {
            id: arcCanvas
            anchors.fill: parent
            onPaint: drawArc()

            // trigger repaints
            property real _hp: arcWindow.highlightPos
            on_HpChanged: requestPaint()

            // preload the brushstroke mask image
            property bool maskLoaded: false
            Component.onCompleted: {
                loadImage("brushstroke-mask.svg");
            }
            onImageLoaded: {
                maskLoaded = true;
                requestPaint();
            }

            function deg2rad(d) { return d * Math.PI / 180; }

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
                var p = ((pos % 1.0) + 1.0) % 1.0;
                var scaled = p * colors.length;
                var i = Math.floor(scaled);
                var t = scaled - i;
                return lerpColor(colors[i % colors.length], colors[(i + 1) % colors.length], t);
            }

            function drawArc() {
                var ctx = getContext("2d");
                var w = arcCanvas.width;
                var h = arcCanvas.height;
                ctx.reset();
                ctx.clearRect(0, 0, w, h);

                var cxc = w / 2;
                var cyc = h / 2;
                var radius = arcWindow.arcRadius;
                var thickness = arcWindow.arcThickness;
                var wsCount = arcWindow.workspaceCount;

                // segment boundaries in the workspace region (t=0..1 maps to segStartDeg..segEndDeg)
                var segSize = 1.0 / wsCount;
                var hlCentre = segSize * (arcWindow.highlightPos + 0.5);
                var hlStart = hlCentre - segSize / 2;
                var hlEnd = hlCentre + segSize / 2;

                // extend first and last workspace highlights into the grey beyond
                var edgeExtendEnd = segSize * 0.7;    // ws5: 70% extra
                var edgeExtendStart = segSize * 2.0;  // ws1: 100% extra (brushstroke extends further)
                if (hlStart <= 0) {
                    hlStart = -edgeExtendStart;
                }
                if (hlEnd >= 1) {
                    hlEnd = 1 + edgeExtendEnd;
                }

                // convert to absolute angles
                var hlStartAngle = arcWindow.segAngle(hlStart);
                var hlEndAngle = arcWindow.segAngle(hlEnd);

                // full 360° fill — first ws colour extends backwards,
                // last ws colour extends forwards, to cover entire ring
                var segEnd = arcWindow.segStartDeg + arcWindow.segSweepDeg;

                ctx.lineWidth = thickness;
                ctx.lineCap = "butt";

                // 1) fill from highlight end, clockwise all the way around
                //    to highlight start — bg3 (covers entire non-highlight region)
                ctx.beginPath();
                ctx.arc(cxc, cyc, radius, deg2rad(hlEndAngle), deg2rad(hlStartAngle + 360), false);
                ctx.strokeStyle = Theme.bg3.toString();
                ctx.stroke();

                // 2) rainbow highlight segment
                var hlX0 = cxc + radius * Math.cos(deg2rad(hlStartAngle));
                var hlY0 = cyc + radius * Math.sin(deg2rad(hlStartAngle));
                var hlX1 = cxc + radius * Math.cos(deg2rad(hlEndAngle));
                var hlY1 = cyc + radius * Math.sin(deg2rad(hlEndAngle));

                var grad = ctx.createLinearGradient(hlX0, hlY0, hlX1, hlY1);
                var gradStops = 8;
                for (var g = 0; g <= gradStops; g++) {
                    var gt = hlStart + (hlEnd - hlStart) * (g / gradStops);
                    var gc = rainbowAt(gt);
                    grad.addColorStop(g / gradStops, Qt.rgba(gc.r, gc.g, gc.b, 1).toString());
                }

                ctx.beginPath();
                ctx.arc(cxc, cyc, radius, deg2rad(hlStartAngle), deg2rad(hlEndAngle), false);
                ctx.strokeStyle = grad;
                ctx.stroke();

                // 3) apply brushstroke texture mask
                if (arcCanvas.maskLoaded) {
                    ctx.globalCompositeOperation = "destination-in";
                    ctx.drawImage("brushstroke-mask.svg", 0, 0, w, h);
                    ctx.globalCompositeOperation = "source-over";
                }
            }
        }

        // ── Workspace numbers along the arc ──
        Repeater {
            model: arcWindow.workspaceCount

            Text {
                required property int index
                property real t: arcWindow.wsT(index)
                property real angle: arcWindow.segAngle(t)
                property bool isActive: Math.abs(arcWindow.highlightPos - index) < 0.5

                x: arcWindow.px(arcWindow.arcRadius * 0.92, angle) - width / 2
                y: arcWindow.py(arcWindow.arcRadius * 0.92, angle) - height / 2

                text: (index + 1).toString()
                color: isActive ? Theme.bg0 : Theme.grey2
                font.family: "JetBrains Mono"
                font.pixelSize: 18
                font.weight: isActive ? Font.Bold : Font.Normal

                Behavior on color {
                    ColorAnimation { duration: 150 }
                }
            }
        }
    }
}
