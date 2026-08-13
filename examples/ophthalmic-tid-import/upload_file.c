/* NON fa parte dell'SDK ufficiale (sdk/c/) — demo separata di come un
 * client in C può mandare un file al server, usando libcurl invece che
 * inventare un client HTTP a mano. Non tocca farsight.h/farsight.c.
 *
 * Build:  cc upload_file.c -o farsight-upload -lcurl
 * Uso:    ./farsight-upload http://<ip-tailscale-server>:8080 default 153210001 /path/al/file.dat
 *
 * Il server salva il file e, se esiste uno script eseguibile in
 * /etc/farsight/importers/<estensione> (qui: /etc/farsight/importers/dat),
 * lo lancia automaticamente — vedi import_tid.py in questa stessa cartella.
 */
#include <curl/curl.h>
#include <stdio.h>
#include <string.h>

int main(int argc, char *argv[]) {
    if (argc != 5) {
        fprintf(stderr, "usage: %s <server-base-url> <tenant_id> <device_id> <file>\n", argv[0]);
        return 1;
    }
    const char *base_url = argv[1];
    const char *tenant_id = argv[2];
    const char *device_id = argv[3];
    const char *file_path = argv[4];

    const char *filename = strrchr(file_path, '/');
    filename = filename ? filename + 1 : file_path;

    FILE *f = fopen(file_path, "rb");
    if (!f) {
        perror("fopen");
        return 1;
    }
    fseek(f, 0, SEEK_END);
    long size = ftell(f);
    fseek(f, 0, SEEK_SET);

    char url[512];
    snprintf(url, sizeof(url), "%s/devices/%s/%s/upload?filename=%s",
             base_url, tenant_id, device_id, filename);

    CURL *curl = curl_easy_init();
    if (!curl) {
        fclose(f);
        fprintf(stderr, "curl_easy_init failed\n");
        return 1;
    }

    curl_easy_setopt(curl, CURLOPT_URL, url);
    curl_easy_setopt(curl, CURLOPT_POST, 1L);
    curl_easy_setopt(curl, CURLOPT_READDATA, f);
    curl_easy_setopt(curl, CURLOPT_POSTFIELDSIZE, size);

    CURLcode res = curl_easy_perform(curl);
    if (res != CURLE_OK) {
        fprintf(stderr, "upload failed: %s\n", curl_easy_strerror(res));
    } else {
        long status = 0;
        curl_easy_getinfo(curl, CURLINFO_RESPONSE_CODE, &status);
        printf("upload complete, HTTP %ld\n", status);
    }

    curl_easy_cleanup(curl);
    fclose(f);
    return res == CURLE_OK ? 0 : 1;
}
