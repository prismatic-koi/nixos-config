import CoreGraphics
import Foundation

guard CommandLine.arguments.count == 2,
      let scalar = CommandLine.arguments[1].unicodeScalars.first
else {
    fputs("usage: macron-type <character>\n", stderr)
    exit(1)
}

guard scalar.value <= 0xFFFF else {
    fputs("error: character must be in the Basic Multilingual Plane (U+0000–U+FFFF)\n", stderr)
    exit(1)
}

let uchar = UniChar(scalar.value)
var keyDown = CGEvent(keyboardEventSource: nil, virtualKey: 0, keyDown: true)!
var keyUp   = CGEvent(keyboardEventSource: nil, virtualKey: 0, keyDown: false)!
keyDown.keyboardSetUnicodeString(stringLength: 1, unicodeString: [uchar])
keyUp.keyboardSetUnicodeString(stringLength: 1,  unicodeString: [uchar])
keyDown.post(tap: CGEventTapLocation(rawValue: 1)!)
keyUp.post(tap: CGEventTapLocation(rawValue: 1)!)
