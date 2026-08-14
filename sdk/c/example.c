/* cc example.c farsight.c -o farsight-example -lpaho-mqtt3c -lcurl */
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

    /* Time series: accumulates in InfluxDB. Nothing is "standard" — CPU
     * here is just an example, pick whatever names make sense. */
    farsight_publish_series(c, "cpu_percent", 12.5);
    farsight_publish_series(c, "patients_visited", 7);

    /* Point value: overwrites in SQLite, no history kept. */
    farsight_set_attribute_string(c, "firmware_version", "1.4.2");
    farsight_set_attribute_double(c, "battery_voltage", 3.7);

    /* Record: one occurrence, accumulates — a device that treats more than
     * one patient over its life keeps one of these per treatment. */
    farsight_field fields[] = {
        {"patient_id", "PID-DEMO-001"},
        {"outcome", "success"},
    };
    farsight_publish_record(c, "TREATMENT-DEMO-001", fields, 2);

    /* Whole file — backups, images, bulk/structured data. saved_name comes
     * back from the server already de-duplicated; linking it to a
     * treatment is just another record, not a built-in relationship —
     * see examples/ophthalmic-tid-import/ for a full worked example. */
    /*
    char saved_name[256];
    if (farsight_upload_file(c, 0, "/path/to/topography.jpg", saved_name, sizeof(saved_name)) == 0) {
        farsight_field image_fields[] = {
            {"filename", saved_name},
            {"treatment_id", "TREATMENT-DEMO-001"},
            {"kind", "topography"},
        };
        farsight_publish_record(c, saved_name, image_fields, 3);
    }
    */

    /* Template: how GET /records renders — a named, overwritable resource,
     * not tied to any one device/record. Push once, then any Grafana panel
     * can select it with ?template=single (see docs/DEVELOPMENT.md). */
    /* farsight_upload_template(c, 0, "single", "/path/to/single.html.tmpl"); */

    printf("published for %s/%s\n", argv[2], argv[3]);

    farsight_disconnect(c);
    return 0;
}
