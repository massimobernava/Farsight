/* cc example.c farsight.c -o farsight-example -lpaho-mqtt3c */
#include "farsight.h"
#include <stdio.h>

int main(int argc, char *argv[]) {
    if (argc != 4) {
        fprintf(stderr, "usage: %s <broker-url> <tenant_id> <device_id>\n", argv[0]);
        return 1;
    }

    farsight_client *c = farsight_connect(argv[1], argv[2], argv[3]);
    if (!c) {
        fprintf(stderr, "farsight_connect failed\n");
        return 1;
    }

    farsight_publish_telemetry(c, 12.5, 40.2, 55.0);
    printf("published telemetry for %s/%s\n", argv[2], argv[3]);

    farsight_disconnect(c);
    return 0;
}
