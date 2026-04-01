import CoreGraphics
import Foundation

// Socket path — must match macron-send
let socketPath = "/tmp/macron-type.sock"

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

func postMacron(_ key: Character) {
    guard let uchar = macrons[key],
          var keyDown = CGEvent(keyboardEventSource: nil, virtualKey: 0, keyDown: true),
          var keyUp   = CGEvent(keyboardEventSource: nil, virtualKey: 0, keyDown: false)
    else { return }
    keyDown.keyboardSetUnicodeString(stringLength: 1, unicodeString: [uchar])
    keyUp.keyboardSetUnicodeString(stringLength: 1,  unicodeString: [uchar])
    keyDown.post(tap: .cgSessionEventTap)
    keyUp.post(tap: .cgSessionEventTap)
}

// Remove stale socket
try? FileManager.default.removeItem(atPath: socketPath)

let serverFd = socket(AF_UNIX, SOCK_STREAM, 0)
guard serverFd >= 0 else {
    fputs("macron-type: socket() failed\n", stderr)
    exit(1)
}

var addr = sockaddr_un()
addr.sun_family = sa_family_t(AF_UNIX)
withUnsafeMutablePointer(to: &addr.sun_path) { ptr in
    socketPath.withCString { cstr in
        _ = strlcpy(UnsafeMutableRawPointer(ptr).assumingMemoryBound(to: CChar.self),
                    cstr, MemoryLayout.size(ofValue: addr.sun_path))
    }
}
let addrLen = socklen_t(MemoryLayout<sockaddr_un>.size)
let bindResult = withUnsafePointer(to: &addr) {
    $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
        bind(serverFd, $0, addrLen)
    }
}
guard bindResult == 0 else {
    fputs("macron-type: bind() failed\n", stderr)
    exit(1)
}
guard listen(serverFd, 16) == 0 else {
    fputs("macron-type: listen() failed\n", stderr)
    exit(1)
}

// Accept connections in a loop
while true {
    let clientFd = accept(serverFd, nil, nil)
    guard clientFd >= 0 else { continue }
    var buf = [UInt8](repeating: 0, count: 1)
    if read(clientFd, &buf, 1) == 1, let key = Character(UnicodeScalar(buf[0])) as Character? {
        postMacron(key)
    }
    close(clientFd)
}
