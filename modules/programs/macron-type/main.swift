import CoreGraphics
import Foundation

let macrons: [Character: UniChar] = [
    "a": 0x0101, // ā
    "e": 0x0113, // ē
    "i": 0x012B, // ī
    "o": 0x014D, // ō
    "u": 0x016B, // ū
    "A": 0x0100, // Ā
    "E": 0x0112, // Ē
    "I": 0x012A, // Ī
    "O": 0x014C, // Ō
    "U": 0x016A, // Ū
]

guard CommandLine.arguments.count == 2,
      let key = CommandLine.arguments[1].first,
      let uchar = macrons[key]
else {
    fputs("usage: macron-type <a|e|i|o|u|A|E|I|O|U>\n", stderr)
    exit(1)
}

var keyDown = CGEvent(keyboardEventSource: nil, virtualKey: 0, keyDown: true)!
var keyUp   = CGEvent(keyboardEventSource: nil, virtualKey: 0, keyDown: false)!
keyDown.keyboardSetUnicodeString(stringLength: 1, unicodeString: [uchar])
keyUp.keyboardSetUnicodeString(stringLength: 1,  unicodeString: [uchar])
keyDown.post(tap: CGEventTapLocation(rawValue: 1)!)
keyUp.post(tap: CGEventTapLocation(rawValue: 1)!)
