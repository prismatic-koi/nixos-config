import Quickshell
import Quickshell.Wayland
import Quickshell.Io
import QtQuick

// Now-playing widget — bottom-left card with animated waveform progress
//
// Top half: a full-width filled waveform bar (flat bottom, wavy top) that
//   acts as a progress indicator.  The played portion is rainbow-coloured
//   and animates when playing; the remaining portion is a muted grey track.
//   Circle dots mark the start and current playback position.
//   Elapsed time sits below-left, total time below-right.
//
// Bottom half: single line "Artist - Title"
//
// Visibility:
//   - Shown on Super hold (showBar / hideBar, same as WorkspaceBar)
//   - Briefly flashes on play/pause/track change, then auto-hides after 3 s
//   - Hidden when no MPD player is active
//
// Data source: polls `mpc` directly (MPD only, not MPRIS) for accurate
// position tracking including seeks.
//
// Usage from shell.qml:
//   NowPlaying { id: nowPlaying }
//   nowPlaying.showBar()
//   nowPlaying.hideBar()

PanelWindow {
    id: root

    // -- configuration --
    readonly property int cardWidth: 450
    readonly property int cardHeight: 120
    readonly property int cardRadius: 10
    readonly property int cardMargin: 30
    readonly property int cardPadding: 15
    readonly property int waveformAreaHeight: 38  // total height reserved for waveform area
    readonly property int barThickness: 8         // flat progress rail height
    readonly property int dotRadius: 9            // circle dot at progress position
    readonly property int dotBorder: 2            // border width around the dot
    readonly property int waveMaxHeight: 12       // max peak height of waves above the bar (~2.5x bar total)
    readonly property real wavePixelLength: 75.0  // fixed wavelength in pixels (prevents wave compression)
    readonly property real rainbowCycles: 2.0

    // -- MPD state (populated by mpc polling) --
    property bool hasPlayer: false
    property bool isPlaying: false
    property string trackTitle: ""
    property string trackArtist: ""
    property string displayText: ""
    property real trackPosition: 0   // seconds elapsed
    property real trackLength: 0     // seconds total
    property real progress: (trackLength > 0) ? Math.min(trackPosition / trackLength, 1.0) : 0

    // -- previous state for flash-on-change detection --
    property string _prevDisplayText: ""
    property bool _prevIsPlaying: false

    function formatTime(seconds) {
        var s = Math.floor(seconds);
        var m = Math.floor(s / 60);
        s = s % 60;
        return m + ":" + (s < 10 ? "0" : "") + s;
    }

    // parse "m:ss" or "mm:ss" to seconds
    function parseTime(str) {
        var parts = str.split(":");
        if (parts.length !== 2) return 0;
        return parseInt(parts[0], 10) * 60 + parseInt(parts[1], 10);
    }

    // -- mpc polling --
    // runs: mpc -f '%artist%\t%title%' status
    // output is 2-3 lines:
    //   line 1: "artist\ttitle"  (or just title if no artist)
    //   line 2: "[playing] #1/5   0:17/2:37 (10%)"  (only if playing/paused)
    //   line 3: "volume: 100%   repeat: off   ..."   (only if playing/paused)
    // if stopped, only line 1 + the status line appear (2 lines total)
    Process {
        id: mpcProc
        command: ["mpc", "-f", "%artist%\t%title%", "status"]
        stdout: StdioCollector {
            onStreamFinished: root.parseMpcOutput(this.text)
        }
    }

    Timer {
        id: pollTimer
        interval: 1000
        repeat: true
        running: true  // always poll so we detect state changes for flash
        onTriggered: mpcProc.running = true
    }

    // also poll immediately on startup
    Component.onCompleted: mpcProc.running = true

    function parseMpcOutput(output) {
        var lines = output.trim().split("\n");
        if (lines.length < 2) {
            // mpd not running or no output
            hasPlayer = false;
            isPlaying = false;
            trackTitle = "";
            trackArtist = "";
            displayText = "";
            trackPosition = 0;
            trackLength = 0;
            checkForFlash();
            return;
        }

        // line 1: track info "artist\ttitle"
        var trackLine = lines[0];
        var tabParts = trackLine.split("\t");
        if (tabParts.length >= 2) {
            trackArtist = tabParts[0];
            trackTitle = tabParts[1];
        } else {
            trackArtist = "";
            trackTitle = trackLine;
        }

        // check if playing/paused (line 2 starts with "[playing]" or "[paused]")
        if (lines.length >= 3 && lines[1].charAt(0) === "[") {
            hasPlayer = true;
            var statusLine = lines[1];
            isPlaying = statusLine.indexOf("[playing]") >= 0;

            // parse elapsed/total: "0:17/2:37"
            var timeMatch = statusLine.match(/(\d+:\d+)\/(\d+:\d+)/);
            if (timeMatch) {
                trackPosition = parseTime(timeMatch[1]);
                trackLength = parseTime(timeMatch[2]);
            }
        } else {
            // stopped
            hasPlayer = false;
            isPlaying = false;
            trackPosition = 0;
            trackLength = 0;
        }

        // build display text
        if (hasPlayer) {
            if (trackArtist !== "")
                displayText = trackArtist + " - " + trackTitle;
            else
                displayText = trackTitle;
        } else {
            displayText = "";
        }

        checkForFlash();
    }

    // hide when MPD stops (last track ends, mpd killed, etc.)
    onHasPlayerChanged: {
        if (!hasPlayer && !holdingSuper) {
            flashTimer.stop();
            showing = false;
            slideProgress = 0;
        }
    }

    function checkForFlash() {
        var textChanged = displayText !== _prevDisplayText && displayText !== "";
        // only flash on resume/start (false→true), not on pause (true→false)
        var resumed = isPlaying && !_prevIsPlaying;
        _prevDisplayText = displayText;
        _prevIsPlaying = isPlaying;

        if (textChanged || resumed) {
            flashBriefly();
        }
    }

    // -- waveform animation --
    property real wavePhase: 0
    FrameAnimation {
        id: waveAnim
        running: root.isPlaying && root.visible
        onTriggered: {
            root.wavePhase += 0.03;
            waveformCanvas.requestPaint();
        }
    }
    // repaint when progress changes even while paused (e.g. seek)
    onProgressChanged: waveformCanvas.requestPaint()

    // -- rainbow shimmer --
    property real gradientOffset: 0
    NumberAnimation on gradientOffset {
        id: shimmerAnim
        from: 0; to: 1
        duration: 4000
        loops: Animation.Infinite
        running: root.showing
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

    // -- wave height helper --
    // pixelX: absolute x position in pixels (drives fixed-wavelength sines)
    // envelopePos: 0..1 across the played portion (drives taper envelope)
    // phase: animation offset
    // The envelope tapers to zero at both edges so the blob rises from the
    // bar and settles back before the dot — but the waves themselves keep a
    // constant spatial frequency regardless of how much has been played.
    function waveHeight(pixelX, envelopePos, phase) {
        // envelope: smooth taper at both ends using a sine window
        var envelope = Math.sin(envelopePos * Math.PI);  // 0 at edges, 1 at center
        envelope = Math.pow(envelope, 1.5);                // steeper taper at edges

        // composite sine waves using fixed pixel wavelength for gentle rolling hills
        var wl = wavePixelLength;
        var y1 = Math.sin(pixelX / wl * Math.PI * 2 + phase) * 0.5;
        var y2 = Math.sin(pixelX / (wl * 1.67) * Math.PI * 2 + phase * 0.7 + 1.0) * 0.35;
        var y3 = Math.sin(pixelX / (wl * 0.67) * Math.PI * 2 + phase * 1.2 + 2.2) * 0.15;

        // combine: shift to 0..1 range (only positive — waves go up from bar)
        var combined = (y1 + y2 + y3 + 1.0) / 2.0;

        return combined * envelope * waveMaxHeight;
    }

    // ghost wave: different frequencies and phase offsets for the background layer
    function waveHeightGhost(pixelX, envelopePos, phase) {
        var envelope = Math.sin(envelopePos * Math.PI);
        envelope = Math.pow(envelope, 1.5);

        var wl = wavePixelLength;
        // offset frequencies and phases so the ghost wave moves independently
        var y1 = Math.sin(pixelX / (wl * 1.2) * Math.PI * 2 + phase * 0.6 + 0.8) * 0.5;
        var y2 = Math.sin(pixelX / (wl * 0.9) * Math.PI * 2 + phase * 1.1 + 2.5) * 0.35;
        var y3 = Math.sin(pixelX / (wl * 1.5) * Math.PI * 2 + phase * 0.4 + 4.0) * 0.15;

        var combined = (y1 + y2 + y3 + 1.0) / 2.0;

        return combined * envelope * waveMaxHeight;
    }

    // -- slide animation (bottom-left diagonal) --
    property bool showing: false
    property real slideProgress: 0
    Behavior on slideProgress {
        NumberAnimation {
            id: slideAnim
            duration: 250
            easing.type: Easing.OutCubic
            onRunningChanged: {
                if (!running && !root.showing && !root.holdingSuper) {
                    root.visible = false;
                }
            }
        }
    }

    // -- Super-hold state --
    property bool holdingSuper: false

    function showBar() {
        if (!hasPlayer) return;
        holdingSuper = true;
        visible = true;
        showing = true;
        slideProgress = 1;
    }
    function hideBar() {
        holdingSuper = false;
        // if the flash timer is still running and we have a player, stay visible
        if (flashTimer.running && hasPlayer) return;
        showing = false;
        slideProgress = 0;
    }

    // -- flash on playback events --
    Timer {
        id: flashTimer
        interval: 5000
        repeat: false
        onTriggered: {
            // only hide if Super isn't being held
            if (!root.holdingSuper) {
                root.showing = false;
                root.slideProgress = 0;
            }
        }
    }

    function flashBriefly() {
        if (!hasPlayer) return;
        visible = true;
        showing = true;
        slideProgress = 1;
        flashTimer.restart();
    }

    // -- window configuration --
    visible: false
    color: "transparent"
    WlrLayershell.layer: WlrLayer.Overlay
    WlrLayershell.namespace: "quickshell-now-playing"
    anchors.bottom: true
    anchors.left: true
    readonly property int travelSize: Math.max(cardMargin + cardWidth, cardMargin + cardHeight)
    width: travelSize + cardWidth
    height: travelSize + cardHeight
    exclusiveZone: 0
    focusable: false
    mask: Region { item: contentRoot }  // only the card intercepts input; rest is click-through

    // -- content --
    Item {
        id: clipRoot
        anchors.fill: parent
        clip: true

        Item {
            id: contentRoot
            width: root.cardWidth
            height: root.cardHeight

            // resting position: bottom-left corner with margin
            x: root.cardMargin
            y: parent.height - root.cardHeight - root.cardMargin

            transform: Translate {
                // slide diagonally from bottom-left: left and down
                x: (1 - root.slideProgress) * -root.travelSize
                y: (1 - root.slideProgress) * root.travelSize
            }

            // card background
            Rectangle {
                id: card
                anchors.fill: parent
                color: Theme.bg3
                radius: root.cardRadius

                // -- waveform progress bar --
                Canvas {
                    id: waveformCanvas
                    x: root.cardPadding
                    y: root.cardPadding
                    width: parent.width - root.cardPadding * 2
                    height: root.waveformAreaHeight

                    // helper: trace a rounded rectangle path (does not fill/stroke)
                    function roundedRectPath(ctx, x, y, w, h, r) {
                        ctx.moveTo(x + r, y);
                        ctx.lineTo(x + w - r, y);
                        ctx.arcTo(x + w, y, x + w, y + r, r);
                        ctx.lineTo(x + w, y + h - r);
                        ctx.arcTo(x + w, y + h, x + w - r, y + h, r);
                        ctx.lineTo(x + r, y + h);
                        ctx.arcTo(x, y + h, x, y + h - r, r);
                        ctx.lineTo(x, y + r);
                        ctx.arcTo(x, y, x + r, y, r);
                    }

                    onPaint: {
                        var ctx = getContext("2d");
                        ctx.clearRect(0, 0, width, height);

                        var w = width;
                        var h = height;
                        var barRadius = root.barThickness / 2;
                        // position the rail so the dot (centered on barCenterY) has room below
                        var dotOverhang = root.dotRadius + root.dotBorder; // how far dot extends from center
                        var barCenterY = h - dotOverhang;  // rail vertical center
                        var barTop = barCenterY - barRadius;
                        var progressX = root.progress * w;
                        var step = 2;
                        var phase = root.wavePhase;
                        var dotR = root.dotRadius;
                        var stops = 8;

                        // --- full-width rounded rail (inactive track) ---
                        ctx.beginPath();
                        roundedRectPath(ctx, 0, barTop, w, root.barThickness, barRadius);
                        ctx.fillStyle = Theme.grey0.toString();
                        ctx.fill();

                        // --- shared rainbow gradient spanning full bar width ---
                        // The gradient covers the entire width; clipping reveals
                        // it progressively so colors don't compress at the start.
                        var rainbowGrad = ctx.createLinearGradient(0, 0, w, 0);
                        for (var si = 0; si <= stops; si++) {
                            var t = si / stops;
                            rainbowGrad.addColorStop(t, root.rainbowAt(t * root.rainbowCycles).toString());
                        }

                        // --- active rail fill (left of progress, coloured) ---
                        if (progressX > 0) {
                            ctx.save();
                            ctx.beginPath();
                            roundedRectPath(ctx, 0, barTop, progressX + barRadius, root.barThickness, barRadius);
                            ctx.clip();

                            ctx.fillStyle = rainbowGrad;
                            ctx.fillRect(0, barTop, progressX + barRadius, root.barThickness);
                            ctx.restore();
                        }

                        // --- ghost waveform (behind, lower opacity) ---
                        if (progressX > dotR * 2 + barRadius) {
                            var waveEnd = progressX - dotR;
                            var waveStart = barRadius;  // start past the rounded bar cap

                            ctx.beginPath();
                            ctx.moveTo(waveStart, barTop);
                            for (var gpx = waveStart; gpx <= waveEnd; gpx += step) {
                                var gEnv = (gpx - waveStart) / (waveEnd - waveStart);
                                var gwh = root.waveHeightGhost(gpx, gEnv, phase);
                                ctx.lineTo(gpx, barTop - gwh);
                            }
                            ctx.lineTo(waveEnd, barTop - root.waveHeightGhost(waveEnd, 1.0, phase));
                            ctx.lineTo(waveEnd, barTop);
                            ctx.closePath();

                            // same full-width gradient but at reduced opacity
                            var ghostGrad = ctx.createLinearGradient(0, 0, w, 0);
                            for (var gi = 0; gi <= stops; gi++) {
                                var gt = gi / stops;
                                var gc = root.rainbowAt(gt * root.rainbowCycles);
                                ghostGrad.addColorStop(gt, Qt.rgba(gc.r, gc.g, gc.b, 0.35).toString());
                            }
                            ctx.fillStyle = ghostGrad;
                            ctx.fill();
                        }

                        // --- main waveform blob sitting on top of the bar ---
                        if (progressX > dotR * 2 + barRadius) {
                            var waveEnd = progressX - dotR;
                            var waveStart = barRadius;  // start past the rounded bar cap
                            ctx.beginPath();
                            ctx.moveTo(waveStart, barTop);
                            for (var px = waveStart; px <= waveEnd; px += step) {
                                var envelopePos = (px - waveStart) / (waveEnd - waveStart);  // 0..1 for envelope taper
                                var wh = root.waveHeight(px, envelopePos, phase);
                                ctx.lineTo(px, barTop - wh);
                            }
                            ctx.lineTo(waveEnd, barTop - root.waveHeight(waveEnd, 1.0, phase));
                            ctx.lineTo(waveEnd, barTop);
                            ctx.closePath();

                            ctx.fillStyle = rainbowGrad;
                            ctx.fill();
                        }

                        // --- circle dot at current progress (centered on rail) ---
                        var dotCenterY = barCenterY;
                        var dotX = Math.max(progressX, dotR + root.dotBorder);
                        // outer border ring
                        ctx.beginPath();
                        ctx.arc(dotX, dotCenterY, dotR + root.dotBorder, 0, Math.PI * 2);
                        ctx.fillStyle = Theme.bg3.toString();
                        ctx.fill();
                        // inner filled dot
                        ctx.beginPath();
                        ctx.arc(dotX, dotCenterY, dotR, 0, Math.PI * 2);
                        ctx.fillStyle = root.rainbowAt(root.progress * root.rainbowCycles).toString();
                        ctx.fill();
                    }
                }

                // -- time labels --
                Text {
                    x: root.cardPadding
                    y: root.cardPadding + root.waveformAreaHeight + 4
                    text: root.hasPlayer ? root.formatTime(root.trackPosition) : ""
                    color: Theme.foreground
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: 14
                }
                Text {
                    x: parent.width - root.cardPadding - width
                    y: root.cardPadding + root.waveformAreaHeight + 4
                    text: root.hasPlayer ? root.formatTime(root.trackLength) : ""
                    color: Theme.foreground
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: 14
                }

                // -- artist - title --
                Text {
                    x: root.cardPadding
                    y: parent.height - root.cardPadding - height
                    width: parent.width - root.cardPadding * 2
                    text: root.displayText
                    color: Theme.foreground
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: 18
                    font.weight: Font.DemiBold
                    elide: Text.ElideRight
                    maximumLineCount: 1
                }
            }
        }
    }
}
