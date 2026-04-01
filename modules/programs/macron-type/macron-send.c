#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#define SOCKET_PATH "/tmp/macron-type.sock"

int main(int argc, char *argv[]) {
    if (argc != 2 || argv[1][0] == '\0') {
        fprintf(stderr, "usage: macron-send <a|e|i|o|u|A|E|I|O|U>\n");
        return 1;
    }

    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0) {
        perror("macron-send: socket");
        return 1;
    }

    struct sockaddr_un addr;
    memset(&addr, 0, sizeof(addr));
    addr.sun_family = AF_UNIX;
    strlcpy(addr.sun_path, SOCKET_PATH, sizeof(addr.sun_path));

    if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        perror("macron-send: connect");
        close(fd);
        return 1;
    }

    char c = argv[1][0];
    write(fd, &c, 1);
    close(fd);
    return 0;
}
