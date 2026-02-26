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
// Outside weather data (temp, wind speed/direction, condition, humidity) comes
// from MetService's public JSON API for Wellington (location ID 93434).
// Two endpoints are fetched in parallel and merged with jq:
//   currentConditions → temp, wind speed (km/h), wind direction (cardinal), humidity
//   twoDayForecast    → current period condition slug (e.g. "partly-cloudy")
// No API key is required; a Referer header is sufficient.
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
    readonly property int cardRadius: 5
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
    property string outsideWindDir: ""     // cardinal string from MetService, e.g. "SE"
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

    // -- MetService/HA condition slug → human-friendly label --
    function friendlyCondition(cond) {
        var c = cond.toLowerCase().replace(/-/g, " ");
        // known multi-word mappings
        if (c === "partlycloudy" || c === "partly cloudy")
            return "partly cloudy";
        if (c === "mostlycloudy" || c === "mostly cloudy")
            return "mostly cloudy";
        if (c === "mostlysunny" || c === "mostly sunny")
            return "mostly sunny";
        if (c === "partlysunny" || c === "partly sunny")
            return "partly sunny";
        if (c === "lightrain" || c === "light rain")
            return "light rain";
        if (c === "heavyrain" || c === "heavy rain")
            return "heavy rain";
        if (c === "lightsnow" || c === "light snow")
            return "light snow";
        if (c === "heavysnow" || c === "heavy snow")
            return "heavy snow";
        if (c === "freezingrain" || c === "freezing rain")
            return "freezing rain";
        if (c === "lightshowers" || c === "light showers")
            return "light showers";
        if (c === "heavyshowers" || c === "heavy showers")
            return "heavy showers";
        if (c === "lightsleet" || c === "light sleet")
            return "light sleet";
        if (c === "heavysleet" || c === "heavy sleet")
            return "heavy sleet";
        // MetService-specific slugs (already space-separated after replace above)
        return c;
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

    // -- wind cardinal string → Unicode directional arrow --
    // dir is what MetService gives us: "N","NNE","NE","ENE","E","ESE","SE","SSE",
    // "S","SSW","SW","WSW","W","WNW","NW","NNW".
    // The arrow shows where wind is blowing TOWARD (opposite of origin direction).
    function windArrow(dir) {
        if (!dir || dir.length === 0)
            return "";
        var pts = ["N","NNE","NE","ENE","E","ESE","SE","SSE","S","SSW","SW","WSW","W","WNW","NW","NNW"];
        // arrows indexed to match pts (0=N blows toward S=↓, etc.)
        var toward = ["↓","↙","↙","↙","←","↖","↖","↖","↑","↗","↗","↗","→","↘","↘","↘"];
        var idx = pts.indexOf(dir.toUpperCase());
        return idx >= 0 ? toward[idx] : "";
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

    // -- MetService polling: outside weather for Wellington --
    // Fetches currentConditions (temp, wind, humidity) and twoDayForecast
    // (condition slug) in one shell pipeline and merges them with jq.
    // The condition is chosen based on the current hour:
    //   00-05 → overnight, 06-11 → morning, 12-17 → afternoon, 18-23 → evening
    Process {
        id: outsideWeatherProc
        command: ["sh", "-c", "HOUR=$(date +%-H); COND=$(curl -sf 'https://www.metservice.com/publicData/webdata/module/twoDayForecast/93434/kelburn_wellington' -H 'Referer: https://www.metservice.com/' | jq -r --argjson h \"$HOUR\" 'if $h < 6 then .days[0].breakdown.overnight.condition elif $h < 12 then .days[0].breakdown.morning.condition elif $h < 18 then .days[0].breakdown.afternoon.condition else .days[0].breakdown.evening.condition end'); curl -sf 'https://www.metservice.com/publicData/webdata/module/currentConditions/93434/93434?pagetype=48hr' -H 'Referer: https://www.metservice.com/' | jq -c --arg cond \"$COND\" '{temp: .observations.temperature[0].current, wind_speed: .observations.wind[0].averageSpeed, wind_dir: .observations.wind[0].direction, humidity: .observations.rain[0].relativeHumidity, condition: $cond}'"]
        stdout: StdioCollector {
            onStreamFinished: {
                try {
                    var obj = JSON.parse(this.text.trim());
                    if (obj.temp !== undefined && obj.temp !== null)
                        root.outsideTemp = obj.temp;
                    if (obj.condition && obj.condition !== "null" && obj.condition !== "unknown")
                        root.outsideCondition = obj.condition;
                    if (obj.wind_speed !== undefined && obj.wind_speed !== null)
                        root.outsideWindSpeed = obj.wind_speed;
                    root.outsideWindDir = (obj.wind_dir && obj.wind_dir !== "null") ? obj.wind_dir : "";
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
    implicitWidth: travelSize + cardWidth
    implicitHeight: travelSize + cardHeight
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
                                        text: root.outsideWindDir.length > 0 ? root.windArrow(root.outsideWindDir) : ""
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 11
                                    }

                                    Text {
                                        text: root.outsideWindDir
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
                                visible: root.outsideWindDir.length > 0
                                spacing: 3
                                anchors.verticalCenter: parent.verticalCenter

                                Text {
                                    text: root.windArrow(root.outsideWindDir)
                                    color: Qt.rgba(Theme.foreground.r, Theme.foreground.g, Theme.foreground.b, 0.6)
                                    font.family: "JetBrainsMono Nerd Font"
                                    font.pixelSize: 11
                                    anchors.verticalCenter: parent.verticalCenter
                                }

                                Text {
                                    text: root.outsideWindDir
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
