import Quickshell
import Quickshell.Wayland
import Quickshell.Io
import QtQuick

// Environment widget — top-right card showing office + outside conditions
//
// Layout:
//
//   ┌──────────────────────┬────────────────────────────┐
//   │  OFFICE              │  OUTSIDE                   │
//   │  23.7°    66.6%      │  18.2°    12 km/h          │
//   │  TEMP     HUMID      │  TEMP     WIND              │
//   │                      │  ☁ partly cloudy   ↗ NE   │
//   └──────────────────────┴────────────────────────────┘
//
// Card width is content-driven: each panel sizes to its content and the
// card expands to fit, so longer condition strings never get truncated.
//
// Outside weather data (temp, wind speed/bearing, condition) all come from
// a single weather.home API call to avoid redundant requests.
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
// Wind speed colour scale (Wellington-calibrated):
//   0–12 km/h  → blue+purple midpoint  (unusually still)
//   12–20 km/h → Theme.aqua            (calm by Wellington standards)
//   20–28 km/h → soft green-white      (moderate, normal)
//   28–35 km/h → Theme.yellow          (getting breezy)
//   35–45 km/h → Theme.orange          (strong, typical Wellington high)
//   45+ km/h   → Theme.red             (genuinely unpleasant)
//
// Visibility:
//   - Shown on Super hold (showBar / hideBar, same as NowPlaying)
//   - Hidden when no data is available
//
// Usage from shell.qml:
//   Environment { id: environment }
//   environment.showBar()
//   environment.hideBar()

PanelWindow {
    id: root

    // -- configuration --
    readonly property int cardHeight: 120
    readonly property int cardRadius: 10
    readonly property int cardMargin: 30
    readonly property int cardPadding: 15
    readonly property int panelSpacing: 10  // gap between data columns within a panel
    readonly property int conditionSpacing: 12 // gap between condition text and wind direction
    // showOffice: true only when this machine is physically in the office
    readonly property bool showOffice: Theme.deviceLocation === "office"
    // cardWidth is derived from the content row's implicitWidth once rendered;
    // seed with a minimum so the window has a valid size before data loads.
    readonly property int cardWidth: Math.max(contentRow.implicitWidth + cardPadding * 2, 180)

    // -- shared vertical row positions (from panel top) --
    // All panels use these so labels/values align across the divider.
    readonly property int rowLabel: 12        // "OFFICE" / "OUTSIDE" header
    readonly property int rowValue: 30        // big number row (top of text)
    readonly property int rowSubLabel: 72     // TEMP / WIND / HUMID sub-labels
    readonly property int rowCondition: 90    // condition + wind direction extra row

    // -- data --
    property real officeTemp: 0
    property real officeHumidity: 0
    property real outsideTemp: 0
    property string outsideCondition: ""
    property real outsideWindSpeed: 0
    property string outsideWindUnit: "km/h"
    property real outsideWindBearing: -1   // -1 = unknown
    property bool hasData: false

    // -- temperature colour helper --
    function tempColor(temp) {
        if (temp <= 8)
            return Qt.rgba((Theme.blue.r + Theme.purple.r) / 2, (Theme.blue.g + Theme.purple.g) / 2, (Theme.blue.b + Theme.purple.b) / 2, 1);
        if (temp <= 15)
            return Theme.aqua;
        if (temp <= 22)
            return Theme.green;
        if (temp <= 25)
            return Theme.yellow;
        return Theme.orange;
    }

    // -- humidity colour helper --
    function humidColor(humid) {
        if (humid <= 0)
            return Qt.rgba((Theme.blue.r + Theme.purple.r) / 2, (Theme.blue.g + Theme.purple.g) / 2, (Theme.blue.b + Theme.purple.b) / 2, 1);
        if (humid <= 35)
            return Theme.aqua;
        if (humid <= 60)
            return Theme.green;
        if (humid <= 75)
            return Theme.yellow;
        if (humid <= 90)
            return Theme.orange;
        return Theme.red;
    }

    // -- wind speed colour helper (Wellington-calibrated) --
    //   0–12   → blue+purple midpoint  (unusually still)
    //   12–20  → Theme.aqua            (calm by Wellington standards)
    //   20–28  → soft green-white      (moderate, normal)
    //   28–35  → Theme.yellow          (getting breezy)
    //   35–45  → Theme.orange          (strong, typical Wellington high)
    //   45+    → Theme.red             (genuinely unpleasant)
    function windColor(speed) {
        if (speed <= 12)
            return Qt.rgba((Theme.blue.r + Theme.purple.r) / 2, (Theme.blue.g + Theme.purple.g) / 2, (Theme.blue.b + Theme.purple.b) / 2, 1);
        if (speed <= 20)
            return Theme.aqua;
        if (speed <= 28)
            return Qt.rgba((Theme.green.r + 1) / 2, (Theme.green.g + 1) / 2, (Theme.green.b + 1) / 2, 1);
        if (speed <= 35)
            return Theme.yellow;
        if (speed <= 45)
            return Theme.orange;
        return Theme.red;
    }

    // -- HA state → human-friendly label --
    function friendlyCondition(cond) {
        var c = cond.toLowerCase();
        if (c === "partlycloudy")
            return "partly cloudy";
        if (c === "mostlycloudy")
            return "mostly cloudy";
        if (c === "mostlysunny")
            return "mostly sunny";
        if (c === "partlysunny")
            return "partly sunny";
        if (c === "lightrain")
            return "light rain";
        if (c === "heavyrain")
            return "heavy rain";
        if (c === "lightsnow")
            return "light snow";
        if (c === "heavysnow")
            return "heavy snow";
        if (c === "freezingrain")
            return "freezing rain";
        if (c === "lightshowers")
            return "light showers";
        if (c === "heavyshowers")
            return "heavy showers";
        if (c === "lightsleet")
            return "light sleet";
        if (c === "heavysleet")
            return "heavy sleet";
        return cond.replace(/[_-]/g, " ");
    }

    // -- weather condition → Nerd Font icon --
    function conditionIcon(cond) {
        var c = cond.toLowerCase();
        if (c.indexOf("thunderstorm") >= 0)
            return "󰙾";
        if (c.indexOf("thunder") >= 0)
            return "󰙾";
        if (c.indexOf("lightning") >= 0)
            return "󰙾";
        if (c.indexOf("hail") >= 0)
            return "󰖒";
        if (c.indexOf("snow") >= 0)
            return "󰖘";
        if (c.indexOf("sleet") >= 0)
            return "󰖘";
        if (c.indexOf("freezing") >= 0)
            return "󰖘";
        if (c.indexOf("drizzle") >= 0)
            return "󰖦";
        if (c.indexOf("shower") >= 0)
            return "󰖦";
        if (c.indexOf("rain") >= 0)
            return "󰖦";
        if (c.indexOf("mist") >= 0)
            return "󰖌";
        if (c.indexOf("fog") >= 0)
            return "󰖌";
        if (c.indexOf("haze") >= 0)
            return "󰖌";
        if (c.indexOf("smoke") >= 0)
            return "󰖌";
        if (c.indexOf("dust") >= 0)
            return "󰖌";
        if (c.indexOf("overcast") >= 0)
            return "󰖐";
        if (c.indexOf("mostly cloudy") >= 0)
            return "󰖐";
        if (c.indexOf("cloudy") >= 0)
            return "󰖐";
        if (c.indexOf("partly") >= 0)
            return "󰖕";
        if (c.indexOf("cloud") >= 0)
            return "󰖕";
        if (c.indexOf("sunny") >= 0)
            return "󰖙";
        if (c.indexOf("clear") >= 0)
            return "󰖙";
        if (c.indexOf("wind") >= 0)
            return "󰖝";
        return "󰖙";
    }

    // -- wind bearing (degrees) → cardinal compass label --
    // 16-point compass; bearing 0/360 = N.
    function windCardinal(deg) {
        if (deg < 0)
            return "";
        var d = ((deg % 360) + 360) % 360;
        var idx = Math.round(d / 22.5) % 16;
        var pts = ["N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"];
        return pts[idx];
    }

    // -- wind bearing → Unicode directional arrow --
    // Bearing is the direction the wind comes FROM. The arrow shows where it is
    // blowing TOWARD, so we add 180° to get the travel direction.
    // e.g. bearing 168° (SSE) → travelling NNW → arrow ↑
    function windArrow(deg) {
        if (deg < 0)
            return "";
        var d = (((deg + 180) % 360) + 360) % 360;
        var idx = Math.round(d / 45) % 8;
        // ↑ ↗ → ↘ ↓ ↙ ← ↖
        var arrows = ["↑", "↗", "→", "↘", "↓", "↙", "←", "↖"];
        return arrows[idx];
    }

    // -- HA polling: office temperature --
    Process {
        id: officeTempProc
        command: ["sh", "-c", "curl -X GET -H \"Authorization: Bearer $(cat /run/secrets/hass_api_key)\" -H \"Content-Type: application/json\" -s \"https://$(cat /run/secrets/hass_domain)/api/states/sensor.office_sensor_temperature\" | jq -r '.state'"]
        stdout: StdioCollector {
            onStreamFinished: {
                var val = parseFloat(this.text.trim());
                if (!isNaN(val))
                    root.officeTemp = val;
                root._checkData();
            }
        }
    }

    // -- HA polling: office humidity --
    Process {
        id: officeHumidityProc
        command: ["sh", "-c", "curl -X GET -H \"Authorization: Bearer $(cat /run/secrets/hass_api_key)\" -H \"Content-Type: application/json\" -s \"https://$(cat /run/secrets/hass_domain)/api/states/sensor.office_sensor_humidity\" | jq -r '.state'"]
        stdout: StdioCollector {
            onStreamFinished: {
                var val = parseFloat(this.text.trim());
                if (!isNaN(val))
                    root.officeHumidity = val;
                root._checkData();
            }
        }
    }

    // -- HA polling: all outside weather from weather.home in one request --
    // Returns JSON with state, temperature, wind_speed, wind_speed_unit, wind_bearing.
    Process {
        id: outsideWeatherProc
        command: ["sh", "-c", "curl -X GET -H \"Authorization: Bearer $(cat /run/secrets/hass_api_key)\" -H \"Content-Type: application/json\" -s \"https://$(cat /run/secrets/hass_domain)/api/states/weather.home\" | jq -c '{state: .state, temp: .attributes.temperature, wind_speed: .attributes.wind_speed, wind_unit: .attributes.wind_speed_unit, wind_bearing: .attributes.wind_bearing}'"]
        stdout: StdioCollector {
            onStreamFinished: {
                try {
                    var obj = JSON.parse(this.text.trim());
                    if (obj.temp !== undefined && obj.temp !== null)
                        root.outsideTemp = obj.temp;
                    if (obj.state && obj.state !== "null" && obj.state !== "unknown")
                        root.outsideCondition = obj.state;
                    if (obj.wind_speed !== undefined && obj.wind_speed !== null)
                        root.outsideWindSpeed = obj.wind_speed;
                    if (obj.wind_unit)
                        root.outsideWindUnit = obj.wind_unit;
                    root.outsideWindBearing = (obj.wind_bearing !== undefined && obj.wind_bearing !== null) ? obj.wind_bearing : -1;
                } catch (e) {}
                root._checkData();
            }
        }
    }

    function _checkData() {
        root.hasData = (root.officeTemp !== 0 || root.officeHumidity !== 0 || root.outsideTemp !== 0);
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
        outsideWeatherProc.running = true;
    }

    // -- slide animation (top-right diagonal) --
    property bool showing: false
    property real slideProgress: 0
    Behavior on slideProgress {
        NumberAnimation {
            duration: 250
            easing.type: Easing.OutCubic
            onRunningChanged: {
                if (!running && !root.showing && !root.holdingSuper)
                    root.visible = false;
            }
        }
    }

    // -- Super-hold state --
    property bool holdingSuper: false

    function showBar() {
        if (!hasData)
            return;
        holdingSuper = true;
        visible = true;
        showing = true;
        slideProgress = 1;
    }
    function hideBar() {
        holdingSuper = false;
        showing = false;
        slideProgress = 0;
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
    mask: Region {
        item: contentRoot
    }

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

            // card background with subtle left→right gradient
            Rectangle {
                anchors.fill: parent
                radius: root.cardRadius
                // base fill; gradient overlay painted on top
                color: Theme.bg3

                // gradient: slightly lighter on right edge
                Rectangle {
                    anchors.fill: parent
                    radius: root.cardRadius
                    gradient: Gradient {
                        orientation: Gradient.Horizontal
                        GradientStop {
                            position: 0.0
                            color: "transparent"
                        }
                        GradientStop {
                            position: 1.0
                            color: Qt.rgba(1, 1, 1, 0.04)
                        }
                    }
                }

                // ── panels laid out in a Row; each panel sizes to its content ──
                Row {
                    id: contentRow
                    anchors.fill: parent
                    spacing: 0

                    // ── LEFT PANEL: Office ────────────────────────────────────────
                    Item {
                        id: officePanel
                        visible: root.showOffice
                        implicitWidth: visible ? officePanelInner.implicitWidth + root.cardPadding * 2 : 0
                        height: parent.height

                        // measure content width via an invisible Column
                        Column {
                            id: officePanelInner
                            visible: false
                            spacing: 0

                            Text {
                                text: "OFFICE"
                                font.family: "JetBrainsMono Nerd Font"
                                font.pixelSize: 11
                                font.letterSpacing: 1
                            }

                            Row {
                                spacing: root.panelSpacing

                                Text {
                                    text: root.officeTemp > 0 ? root.officeTemp.toFixed(1) + "°" : "—"
                                    font.family: "JetBrainsMono Nerd Font"
                                    font.pixelSize: 30
                                    font.weight: Font.DemiBold
                                }

                                Text {
                                    text: root.officeHumidity > 0 ? root.officeHumidity.toFixed(1) + "%" : "—"
                                    font.family: "JetBrainsMono Nerd Font"
                                    font.pixelSize: 30
                                    font.weight: Font.DemiBold
                                }
                            }
                        }

                        // "OFFICE" header
                        Text {
                            x: root.cardPadding
                            y: root.rowLabel
                            text: "OFFICE"
                            color: Theme.foreground
                            font.family: "JetBrainsMono Nerd Font"
                            font.pixelSize: 11
                            font.weight: Font.Medium
                            font.letterSpacing: 1
                        }

                        // temperature value
                        Text {
                            id: officeTempVal
                            x: root.cardPadding
                            y: root.rowValue
                            text: root.officeTemp > 0 ? root.officeTemp.toFixed(1) + "°" : "—"
                            color: root.tempColor(root.officeTemp)
                            font.family: "JetBrainsMono Nerd Font"
                            font.pixelSize: 30
                            font.weight: Font.DemiBold
                        }

                        // TEMP sub-label
                        Text {
                            x: root.cardPadding
                            y: root.rowSubLabel
                            text: "TEMP"
                            color: Qt.rgba(Theme.foreground.r, Theme.foreground.g, Theme.foreground.b, 0.5)
                            font.family: "JetBrainsMono Nerd Font"
                            font.pixelSize: 11
                            font.letterSpacing: 1
                        }

                        // humidity value (right of temp)
                        Text {
                            id: officeHumidVal
                            x: officeTempVal.x + officeTempVal.width + root.panelSpacing
                            y: root.rowValue
                            text: root.officeHumidity > 0 ? root.officeHumidity.toFixed(1) + "%" : "—"
                            color: root.humidColor(root.officeHumidity)
                            font.family: "JetBrainsMono Nerd Font"
                            font.pixelSize: 30
                            font.weight: Font.DemiBold
                        }

                        // HUMID sub-label
                        Text {
                            x: officeHumidVal.x
                            y: root.rowSubLabel
                            text: "HUMID"
                            color: Qt.rgba(Theme.foreground.r, Theme.foreground.g, Theme.foreground.b, 0.5)
                            font.family: "JetBrainsMono Nerd Font"
                            font.pixelSize: 11
                            font.letterSpacing: 1
                        }
                    }

                    // ── vertical divider ──────────────────────────────────────────
                    Rectangle {
                        visible: root.showOffice
                        width: visible ? 3 : 0
                        height: root.cardHeight - root.cardPadding * 2
                        anchors.verticalCenter: parent.verticalCenter
                        color: Theme.grey0
                        opacity: 0.6
                    }

                    // ── RIGHT PANEL: Outside ──────────────────────────────────────
                    Item {
                        id: outsidePanel
                        implicitWidth: outsidePanelInner.implicitWidth + root.cardPadding * 2
                        height: parent.height

                        // measure content width via an invisible Column
                        Column {
                            id: outsidePanelInner
                            visible: false
                            spacing: 0

                            Text {
                                text: "OUTSIDE"
                                font.family: "JetBrainsMono Nerd Font"
                                font.pixelSize: 11
                                font.letterSpacing: 1
                            }

                            Row {
                                spacing: root.panelSpacing

                                Text {
                                    text: root.outsideTemp !== 0 ? root.outsideTemp.toFixed(1) + "°" : "—"
                                    font.family: "JetBrainsMono Nerd Font"
                                    font.pixelSize: 30
                                    font.weight: Font.DemiBold
                                }

                                Row {
                                    spacing: 4

                                    Text {
                                        text: root.outsideWindSpeed > 0 ? Math.round(root.outsideWindSpeed).toString() : "—"
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 30
                                        font.weight: Font.DemiBold
                                    }

                                    Text {
                                        text: root.outsideWindSpeed > 0 ? root.outsideWindUnit : ""
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 13
                                    }
                                }
                            }

                            Row {
                                spacing: root.conditionSpacing

                                Row {
                                    spacing: 5

                                    Text {
                                        text: root.outsideCondition.length > 0 ? root.conditionIcon(root.outsideCondition) : ""
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 13
                                    }

                                    Text {
                                        text: root.outsideCondition.length > 0 ? root.friendlyCondition(root.outsideCondition) : ""
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 11
                                        font.letterSpacing: 1
                                    }
                                }

                                Row {
                                    spacing: 3

                                    Text {
                                        text: root.outsideWindBearing >= 0 ? root.windArrow(root.outsideWindBearing) : ""
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 11
                                    }

                                    Text {
                                        text: root.outsideWindBearing >= 0 ? root.windCardinal(root.outsideWindBearing) : ""
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 11
                                        font.letterSpacing: 1
                                    }
                                }
                            }
                        }

                        // "OUTSIDE" header
                        Text {
                            x: root.cardPadding
                            y: root.rowLabel
                            text: "OUTSIDE"
                            color: Theme.foreground
                            font.family: "JetBrainsMono Nerd Font"
                            font.pixelSize: 11
                            font.weight: Font.Medium
                            font.letterSpacing: 1
                        }

                        // outside temperature value
                        Text {
                            id: outsideTempVal
                            x: root.cardPadding
                            y: root.rowValue
                            text: root.outsideTemp !== 0 ? root.outsideTemp.toFixed(1) + "°" : "—"
                            color: root.tempColor(root.outsideTemp)
                            font.family: "JetBrainsMono Nerd Font"
                            font.pixelSize: 30
                            font.weight: Font.DemiBold
                        }

                        // TEMP sub-label
                        Text {
                            x: root.cardPadding
                            y: root.rowSubLabel
                            text: "TEMP"
                            color: Qt.rgba(Theme.foreground.r, Theme.foreground.g, Theme.foreground.b, 0.5)
                            font.family: "JetBrainsMono Nerd Font"
                            font.pixelSize: 11
                            font.letterSpacing: 1
                        }

                        // wind speed value (right of temp)
                        Row {
                            id: windValueRow
                            x: outsideTempVal.x + outsideTempVal.width + root.panelSpacing
                            y: root.rowValue
                            spacing: 4

                            Text {
                                text: root.outsideWindSpeed > 0 ? Math.round(root.outsideWindSpeed).toString() : "—"
                                color: root.windColor(root.outsideWindSpeed)
                                font.family: "JetBrainsMono Nerd Font"
                                font.pixelSize: 30
                                font.weight: Font.DemiBold
                                anchors.verticalCenter: parent.verticalCenter
                            }

                            Text {
                                text: root.outsideWindSpeed > 0 ? root.outsideWindUnit : ""
                                color: Qt.rgba(Theme.foreground.r, Theme.foreground.g, Theme.foreground.b, 0.45)
                                font.family: "JetBrainsMono Nerd Font"
                                font.pixelSize: 13
                                font.weight: Font.Medium
                                anchors.verticalCenter: parent.verticalCenter
                            }
                        }

                        // WIND sub-label
                        Text {
                            x: windValueRow.x
                            y: root.rowSubLabel
                            text: "WIND"
                            color: Qt.rgba(Theme.foreground.r, Theme.foreground.g, Theme.foreground.b, 0.5)
                            font.family: "JetBrainsMono Nerd Font"
                            font.pixelSize: 11
                            font.letterSpacing: 1
                        }

                        // condition icon + friendly text + wind direction — extra row below
                        Row {
                            x: root.cardPadding
                            y: root.rowCondition
                            spacing: root.conditionSpacing

                            Row {
                                spacing: 5
                                anchors.verticalCenter: parent.verticalCenter

                                Text {
                                    text: root.outsideCondition.length > 0 ? root.conditionIcon(root.outsideCondition) : ""
                                    color: Qt.rgba(Theme.foreground.r, Theme.foreground.g, Theme.foreground.b, 0.6)
                                    font.family: "JetBrainsMono Nerd Font"
                                    font.pixelSize: 13
                                    anchors.verticalCenter: parent.verticalCenter
                                }

                                Text {
                                    text: root.outsideCondition.length > 0 ? root.friendlyCondition(root.outsideCondition) : ""
                                    color: Qt.rgba(Theme.foreground.r, Theme.foreground.g, Theme.foreground.b, 0.6)
                                    font.family: "JetBrainsMono Nerd Font"
                                    font.pixelSize: 11
                                    font.letterSpacing: 1
                                    anchors.verticalCenter: parent.verticalCenter
                                }
                            }

                            Row {
                                visible: root.outsideWindBearing >= 0
                                spacing: 3
                                anchors.verticalCenter: parent.verticalCenter

                                Text {
                                    text: root.windArrow(root.outsideWindBearing)
                                    color: Qt.rgba(Theme.foreground.r, Theme.foreground.g, Theme.foreground.b, 0.6)
                                    font.family: "JetBrainsMono Nerd Font"
                                    font.pixelSize: 11
                                    anchors.verticalCenter: parent.verticalCenter
                                }

                                Text {
                                    text: root.windCardinal(root.outsideWindBearing)
                                    color: Qt.rgba(Theme.foreground.r, Theme.foreground.g, Theme.foreground.b, 0.6)
                                    font.family: "JetBrainsMono Nerd Font"
                                    font.pixelSize: 11
                                    font.letterSpacing: 1
                                    anchors.verticalCenter: parent.verticalCenter
                                }
                            }
                        }
                    }
                }
            }
        }
    }
}
