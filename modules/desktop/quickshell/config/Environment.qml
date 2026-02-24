import Quickshell
import Quickshell.Wayland
import Quickshell.Io
import QtQuick

// Environment widget — top-right card showing office + outside conditions
//
// Layout:
//
//   ┌──────────────────────┬──────────────────────┐
//   │  OFFICE              │  OUTSIDE             │
//   │  23.7°    66.6%      │  23.5°    ☁          │
//   │  TEMP     HUMID      │  TEMP     partly cloudy│
//   └──────────────────────┴──────────────────────┘
//
// Card width is content-driven: each panel sizes to its content and the
// card expands to fit, so longer condition strings never get truncated.
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
    readonly property int panelSpacing: 10  // gap between temp and humidity/icon columns
    // showOffice: true only when this machine is physically in the office
    readonly property bool showOffice: Theme.deviceLocation === "office"
    // cardWidth is derived from the content row's implicitWidth once rendered;
    // seed with a minimum so the window has a valid size before data loads.
    readonly property int cardWidth: Math.max(contentRow.implicitWidth + cardPadding * 2, 180)

    // -- data --
    property real officeTemp: 0
    property real officeHumidity: 0
    property real outsideTemp: 0
    property string outsideCondition: ""
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

    // -- HA polling: outside temperature --
    Process {
        id: outsideTempProc
        command: ["sh", "-c", "curl -X GET -H \"Authorization: Bearer $(cat /run/secrets/hass_api_key)\" -H \"Content-Type: application/json\" -s \"https://$(cat /run/secrets/hass_domain)/api/states/sensor.outside_temperature\" | jq -r '.state'"]
        stdout: StdioCollector {
            onStreamFinished: {
                var val = parseFloat(this.text.trim());
                if (!isNaN(val)) {
                    root.outsideTemp = val;
                    root._checkData();
                }
            }
        }
    }

    // -- HA polling: weather condition from weather.home entity --
    Process {
        id: outsideConditionProc
        command: ["sh", "-c", "curl -X GET -H \"Authorization: Bearer $(cat /run/secrets/hass_api_key)\" -H \"Content-Type: application/json\" -s \"https://$(cat /run/secrets/hass_domain)/api/states/weather.home\" | jq -r '.state'"]
        stdout: StdioCollector {
            onStreamFinished: {
                var s = this.text.trim();
                if (s.length > 0 && s !== "null" && s !== "unknown")
                    root.outsideCondition = s;
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

            // card background
            Rectangle {
                anchors.fill: parent
                color: Theme.bg3
                radius: root.cardRadius

                // ── panels laid out in a Row; each panel Column sizes to its content ──
                Row {
                    id: contentRow
                    anchors.centerIn: parent
                    spacing: 0

                    // ── LEFT PANEL: Office ────────────────────────────────────────
                    Item {
                        id: officePanel
                        visible: root.showOffice
                        // size to content when visible, zero when hidden so Row ignores it
                        implicitWidth: visible ? officePanelColumn.implicitWidth + root.cardPadding * 2 : 0
                        implicitHeight: root.cardHeight

                        Column {
                            id: officePanelColumn
                            x: root.cardPadding
                            anchors.verticalCenter: parent.verticalCenter
                            spacing: 2

                            Text {
                                text: "OFFICE"
                                color: Theme.foreground
                                font.family: "JetBrainsMono Nerd Font"
                                font.pixelSize: 11
                                font.weight: Font.Medium
                                font.letterSpacing: 1
                            }

                            Row {
                                spacing: root.panelSpacing

                                Column {
                                    spacing: 2

                                    Text {
                                        id: officeTempText
                                        text: root.officeTemp > 0 ? root.officeTemp.toFixed(1) + "°" : "—"
                                        color: root.tempColor(root.officeTemp)
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 30
                                        font.weight: Font.DemiBold
                                    }

                                    Text {
                                        text: "TEMP"
                                        color: Theme.foreground
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 11
                                        font.letterSpacing: 1
                                    }
                                }

                                Column {
                                    spacing: 2

                                    Text {
                                        text: root.officeHumidity > 0 ? root.officeHumidity.toFixed(1) + "%" : "—"
                                        color: root.humidColor(root.officeHumidity)
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 30
                                        font.weight: Font.DemiBold
                                    }

                                    Text {
                                        text: "HUMID"
                                        color: Theme.foreground
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 11
                                        font.letterSpacing: 1
                                    }
                                }
                            }
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
                        implicitWidth: outsidePanelColumn.implicitWidth + root.cardPadding * 2
                        implicitHeight: root.cardHeight

                        Column {
                            id: outsidePanelColumn
                            x: root.cardPadding
                            anchors.verticalCenter: parent.verticalCenter
                            spacing: 2

                            Text {
                                text: "OUTSIDE"
                                color: Theme.foreground
                                font.family: "JetBrainsMono Nerd Font"
                                font.pixelSize: 11
                                font.weight: Font.Medium
                                font.letterSpacing: 1
                            }

                            Row {
                                spacing: root.panelSpacing

                                Column {
                                    spacing: 2

                                    Text {
                                        text: root.outsideTemp !== 0 ? root.outsideTemp.toFixed(1) + "°" : "—"
                                        color: root.tempColor(root.outsideTemp)
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 30
                                        font.weight: Font.DemiBold
                                    }

                                    Text {
                                        text: "TEMP"
                                        color: Theme.foreground
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 11
                                        font.letterSpacing: 1
                                    }
                                }

                                Column {
                                    spacing: 2

                                    Text {
                                        text: root.outsideCondition.length > 0 ? root.conditionIcon(root.outsideCondition) : ""
                                        color: root.tempColor(root.outsideTemp)
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 30
                                    }

                                    Text {
                                        text: root.outsideCondition.length > 0 ? root.friendlyCondition(root.outsideCondition) : ""
                                        color: Theme.foreground
                                        font.family: "JetBrainsMono Nerd Font"
                                        font.pixelSize: 11
                                        font.letterSpacing: 1
                                    }
                                }
                            }
                        }
                    }
                }
            }
        }
    }
}
