import Quickshell
import Quickshell.Wayland
import Quickshell.Io
import QtQuick

// Environment widget — top-right card showing office + outside conditions
//
// Layout (mirrors the NowPlaying card size, anchored top-right):
//
//   ┌────────────────────────────────────────────────┐
//   │  OFFICE            │  OUTSIDE                  │
//   │  23.7°    66.6%    │  23.5°    ☁               │
//   │  TEMP     HUMID    │  TEMP     light rain       │
//   └────────────────────┴──────────────────────────┘
//
// Temperature colour scale (both office and outside):
//   ≤ 8°C    → blue+purple midpoint  (cold winter morning)
//   ≤ 15°C   → Theme.aqua            (cool, typical winter day)
//   ≤ 22°C   → Theme.green           (comfortable)
//   ≤ 25°C   → Theme.yellow          (warm summer)
//   > 25°C   → Theme.orange          (hot)
//
// Humidity colour scale (office humidity):
//   0%       → blue+purple midpoint  (bone dry)
//   ≤ 35%    → Theme.aqua
//   ≤ 60%    → Theme.green           (comfortable)
//   ≤ 75%    → Theme.yellow          (getting muggy)
//   ≤ 90%    → Theme.orange
//   > 90%    → Theme.red
//
// Weather condition icons use Nerd Font glyphs matched against HA state text.
//
// Visibility:
//   - Shown on Super hold (showBar / hideBar, same as NowPlaying)
//   - Briefly flashes for 5 s when data changes
//   - Hidden when no data is available
//
// Usage from shell.qml:
//   Environment { id: environment }
//   environment.showBar()
//   environment.hideBar()

PanelWindow {
    id: root

    // -- configuration --
    readonly property int cardWidth: 450
    readonly property int cardHeight: 120
    readonly property int cardRadius: 10
    readonly property int cardMargin: 30
    readonly property int cardPadding: 15
    readonly property int dividerX: cardWidth / 2
    // showOffice: true only when this machine is physically in the office
    readonly property bool showOffice: Theme.deviceLocation === "office"
    // right panel starts at divider when office is shown, or at 0 for full-width
    readonly property int rightPanelX: showOffice ? dividerX : 0

    // -- data --
    property real officeTemp: 0
    property real officeHumidity: 0
    property real outsideTemp: 0
    property string outsideCondition: ""
    property bool hasData: false

    // -- previous state for flash detection --
    property real _prevOutsideTemp: -999

    // -- temperature colour helper --
    // Maps temperature bands to theme colours (solid blocks, no gradient).
    function tempColor(temp) {
        if (temp <= 8)  return Qt.rgba(
                            (Theme.blue.r  + Theme.purple.r) / 2,
                            (Theme.blue.g  + Theme.purple.g) / 2,
                            (Theme.blue.b  + Theme.purple.b) / 2,
                            1);
        if (temp <= 15) return Theme.aqua;
        if (temp <= 22) return Theme.green;
        if (temp <= 25) return Theme.yellow;
        return Theme.orange;
    }

    // -- humidity colour helper --
    // Maps humidity bands to theme colours (solid blocks, no gradient).
    function humidColor(humid) {
        if (humid <= 0)  return Qt.rgba(
                             (Theme.blue.r  + Theme.purple.r) / 2,
                             (Theme.blue.g  + Theme.purple.g) / 2,
                             (Theme.blue.b  + Theme.purple.b) / 2,
                             1);
        if (humid <= 35) return Theme.aqua;
        if (humid <= 60) return Theme.green;
        if (humid <= 75) return Theme.yellow;
        if (humid <= 90) return Theme.orange;
        return Theme.red;
    }

    // -- weather condition → Nerd Font icon --
    function conditionIcon(cond) {
        var c = cond.toLowerCase();
        if (c.indexOf("thunderstorm") >= 0) return "󰙾";
        if (c.indexOf("thunder")      >= 0) return "󰙾";
        if (c.indexOf("lightning")    >= 0) return "󰙾";
        if (c.indexOf("hail")         >= 0) return "󰖒";
        if (c.indexOf("snow")         >= 0) return "󰖘";
        if (c.indexOf("sleet")        >= 0) return "󰖘";
        if (c.indexOf("freezing")     >= 0) return "󰖘";
        if (c.indexOf("drizzle")      >= 0) return "󰖦";
        if (c.indexOf("shower")       >= 0) return "󰖦";
        if (c.indexOf("rain")         >= 0) return "󰖦";
        if (c.indexOf("mist")         >= 0) return "󰖌";
        if (c.indexOf("fog")          >= 0) return "󰖌";
        if (c.indexOf("haze")         >= 0) return "󰖌";
        if (c.indexOf("smoke")        >= 0) return "󰖌";
        if (c.indexOf("dust")         >= 0) return "󰖌";
        if (c.indexOf("overcast")     >= 0) return "󰖐";
        if (c.indexOf("cloudy")       >= 0) return "󰖐";
        if (c.indexOf("partly")       >= 0) return "󰖕";
        if (c.indexOf("mostly cloudy")>= 0) return "󰖐";
        if (c.indexOf("cloud")        >= 0) return "󰖕";
        if (c.indexOf("sunny")        >= 0) return "󰖙";
        if (c.indexOf("clear")        >= 0) return "󰖙";
        if (c.indexOf("wind")         >= 0) return "󰖝";
        return "󰖙";  // default: sun
    }

    // -- HA polling: office temperature --
    // Reads secrets directly from /run/secrets/ to avoid systemd env var issues.
    Process {
        id: officeTempProc
        command: [
            "sh", "-c",
            "curl -X GET -H \"Authorization: Bearer $(cat /run/secrets/hass_api_key)\" -H \"Content-Type: application/json\" -s \"https://$(cat /run/secrets/hass_domain)/api/states/sensor.office_sensor_temperature\" | jq -r '.state'"
        ]
        stdout: StdioCollector {
            onStreamFinished: {
                var val = parseFloat(this.text.trim());
                if (!isNaN(val)) root.officeTemp = val;
                root._checkData();
            }
        }
    }

    // -- HA polling: office humidity --
    Process {
        id: officeHumidityProc
        command: [
            "sh", "-c",
            "curl -X GET -H \"Authorization: Bearer $(cat /run/secrets/hass_api_key)\" -H \"Content-Type: application/json\" -s \"https://$(cat /run/secrets/hass_domain)/api/states/sensor.office_sensor_humidity\" | jq -r '.state'"
        ]
        stdout: StdioCollector {
            onStreamFinished: {
                var val = parseFloat(this.text.trim());
                if (!isNaN(val)) root.officeHumidity = val;
                root._checkData();
            }
        }
    }

    // -- HA polling: outside temperature (sensor.outside_temperature) --
    Process {
        id: outsideTempProc
        command: [
            "sh", "-c",
            "curl -X GET -H \"Authorization: Bearer $(cat /run/secrets/hass_api_key)\" -H \"Content-Type: application/json\" -s \"https://$(cat /run/secrets/hass_domain)/api/states/sensor.outside_temperature\" | jq -r '.state'"
        ]
        stdout: StdioCollector {
            onStreamFinished: {
                var val = parseFloat(this.text.trim());
                if (!isNaN(val)) {
                    root.outsideTemp = val;
                    root._checkData();
                    root._checkFlash();
                }
            }
        }
    }

    // -- HA polling: weather condition from weather.home entity --
    // Uses the current condition from the weather entity state directly
    Process {
        id: outsideConditionProc
        command: [
            "sh", "-c",
            "curl -X GET -H \"Authorization: Bearer $(cat /run/secrets/hass_api_key)\" -H \"Content-Type: application/json\" -s \"https://$(cat /run/secrets/hass_domain)/api/states/weather.home\" | jq -r '.state'"
        ]
        stdout: StdioCollector {
            onStreamFinished: {
                var s = this.text.trim();
                if (s.length > 0 && s !== "null" && s !== "unknown") root.outsideCondition = s;
                root._checkData();
            }
        }
    }

    function _checkData() {
        root.hasData = (root.officeTemp !== 0 || root.officeHumidity !== 0 || root.outsideTemp !== 0);
    }

    function _checkFlash() {
        if (root.outsideTemp !== root._prevOutsideTemp && root.hasData) {
            root._prevOutsideTemp = root.outsideTemp;
            root.flashBriefly();
        }
    }

    // -- poll timer (60 s refresh) --
    Timer {
        id: pollTimer
        interval: 60000
        repeat: true
        running: true
        onTriggered: root._poll()
    }

    // stagger on startup so all four requests don't fire simultaneously
    Timer {
        id: startupTimer
        interval: 100
        repeat: false
        running: true
        onTriggered: root._poll()
    }

    function _poll() {
        officeTempProc.running = true;
        officeHumidityProc.running = true;
        outsideTempProc.running = true;
        outsideConditionProc.running = true;
    }

    // -- slide animation (top-right diagonal) --
    property bool showing: false
    property real slideProgress: 0
    Behavior on slideProgress {
        NumberAnimation {
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
        if (!hasData) return;
        holdingSuper = true;
        visible = true;
        showing = true;
        slideProgress = 1;
    }
    function hideBar() {
        holdingSuper = false;
        if (flashTimer.running && hasData) return;
        showing = false;
        slideProgress = 0;
    }

    // -- flash on data change --
    Timer {
        id: flashTimer
        interval: 5000
        repeat: false
        onTriggered: {
            if (!root.holdingSuper) {
                root.showing = false;
                root.slideProgress = 0;
            }
        }
    }

    function flashBriefly() {
        if (!hasData) return;
        visible = true;
        showing = true;
        slideProgress = 1;
        flashTimer.restart();
    }

    // -- window configuration --
    visible: false
    color: "transparent"
    WlrLayershell.layer: WlrLayer.Overlay
    WlrLayershell.namespace: "quickshell-environment"
    anchors.top: true
    anchors.right: true
    readonly property int travelSize: Math.max(cardMargin + cardWidth, cardMargin + cardHeight)
    width: travelSize + cardWidth
    height: travelSize + cardHeight
    exclusiveZone: 0
    focusable: false
    mask: Region { item: contentRoot }

    // -- content --
    Item {
        id: clipRoot
        anchors.fill: parent
        clip: true

        Item {
            id: contentRoot
            width: root.cardWidth
            height: root.cardHeight

            // resting position: top-right corner with margin (waybar is 40px + 30px margin = 70px offset)
            x: parent.width - root.cardWidth - root.cardMargin
            y: root.cardMargin + 40 + root.cardMargin

            transform: Translate {
                // slide diagonally from top-right: right and up
                x: (1 - root.slideProgress) * root.travelSize
                y: (1 - root.slideProgress) * -root.travelSize
            }

            // card background
            Rectangle {
                anchors.fill: parent
                color: Theme.bg3
                radius: root.cardRadius

                // ── LEFT PANEL: Office (only shown when deviceLocation == "office") ──

                Item {
                    visible: root.showOffice

                    // OFFICE label
                    Text {
                        x: root.cardPadding
                        y: root.cardPadding - 1
                        text: "OFFICE"
                        color: Theme.foreground
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: 11
                        font.weight: Font.Medium
                        font.letterSpacing: 1
                    }

                    // office temperature value
                    Text {
                        id: officeTempText
                        x: root.cardPadding
                        y: root.cardPadding + 20
                        text: root.officeTemp > 0 ? root.officeTemp.toFixed(1) + "°" : "—"
                        color: root.tempColor(root.officeTemp)
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: 30
                        font.weight: Font.DemiBold
                    }

                    // office humidity value (sits to the right of temp)
                    Text {
                        id: officeHumidText
                        x: officeTempText.x + officeTempText.width + 10
                        y: officeTempText.y
                        text: root.officeHumidity > 0 ? root.officeHumidity.toFixed(1) + "%" : "—"
                        color: root.humidColor(root.officeHumidity)
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: 30
                        font.weight: Font.DemiBold
                    }

                    // TEMP label under office temp
                    Text {
                        x: officeTempText.x
                        y: officeTempText.y + officeTempText.height - 2
                        text: "TEMP"
                        color: Theme.foreground
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: 11
                        font.letterSpacing: 1
                    }

                    // HUMID label under humidity
                    Text {
                        x: officeHumidText.x
                        y: officeHumidText.y + officeHumidText.height - 2
                        text: "HUMID"
                        color: Theme.foreground
                        font.family: "JetBrainsMono Nerd Font"
                        font.pixelSize: 11
                        font.letterSpacing: 1
                    }
                }

                // ── vertical divider (only when office panel is shown) ─────────
                Rectangle {
                    visible: root.showOffice
                    x: root.dividerX - 1
                    y: root.cardPadding
                    width: 3
                    height: root.cardHeight - root.cardPadding * 2
                    color: Theme.grey0
                    opacity: 0.6
                }

                // ── RIGHT PANEL: Outside ───────────────────────────────────────

                // OUTSIDE label
                Text {
                    x: root.rightPanelX + root.cardPadding
                    y: root.cardPadding - 1
                    text: "OUTSIDE"
                    color: Theme.foreground
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: 11
                    font.weight: Font.Medium
                    font.letterSpacing: 1
                }

                // outside temperature value
                Text {
                    id: outsideTempText
                    x: root.rightPanelX + root.cardPadding
                    y: root.cardPadding + 20
                    text: root.outsideTemp !== 0 ? root.outsideTemp.toFixed(1) + "°" : "—"
                    color: root.tempColor(root.outsideTemp)
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: 30
                    font.weight: Font.DemiBold
                }

                // weather condition icon (right column, same row as temp — mirrors humidity slot)
                Text {
                    id: outsideIconText
                    x: outsideTempText.x + outsideTempText.width + 10
                    y: outsideTempText.y
                    text: root.outsideCondition.length > 0 ? root.conditionIcon(root.outsideCondition) : ""
                    color: root.tempColor(root.outsideTemp)
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: 30
                }

                // TEMP label under outside temp
                Text {
                    x: outsideTempText.x
                    y: outsideTempText.y + outsideTempText.height - 2
                    text: "TEMP"
                    color: Theme.foreground
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: 11
                    font.letterSpacing: 1
                }

                // condition text (e.g. "light rain") — below TEMP label, mirrors HUMID position
                Text {
                    x: outsideIconText.x
                    y: outsideIconText.y + outsideIconText.height - 2
                    width: root.cardWidth / 2 - root.cardPadding * 2
                    text: root.outsideCondition
                    color: Theme.foreground
                    font.family: "JetBrainsMono Nerd Font"
                    font.pixelSize: 11
                    font.letterSpacing: 1
                    elide: Text.ElideRight
                    maximumLineCount: 1
                }
            }
        }
    }
}
