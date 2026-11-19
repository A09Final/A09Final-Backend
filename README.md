# BeforeFire Backend

Backend Go untuk BeforeFire. Tugasnya tiga:

1. **Meneruskan alert** dari alat sensor (ESP32-S3) ke iPhone lewat APNs.
2. **Menyediakan data sensor terkini** (telemetry) untuk layar Monitor di app.
3. **Meneruskan perintah** dari app (misalnya matikan alarm) ke alat sensor, lalu menunggu jawabannya.

Backend ini **stateless**: tanpa database dan tanpa login. Push token HP disimpan di NVS alat sensor dan ikut dikirim di setiap alert. Yang disimpan di memori hanya ID alert terakhir (supaya alert yang sama tidak terkirim dua kali), status online/offline alat, dan data sensor terakhir. Status dan data sensor dikirim alat sebagai pesan *retained*, jadi broker memberikannya lagi setelah backend restart.

```
                 ┌──────────── Mac (Docker) ────────────┐
ESP32-S3 ──MQTT :1883──▶ Mosquitto ──▶ backend (Go) ──HTTPS──▶ APNs (Apple) ──▶ iPhone
   ▲                                     │      ▲
   └──────────── perintah (cmd) ◀────────┘      └── HTTP :8080 ◀── app BeforeFire
ESP32-C3 (alarm) tetap menerima perintah dari ESP32-S3 lewat Wi-Fi lokal, tidak lewat backend.
```

Server berjalan di Mac. ESP32 dan app tersambung ke nama `beforefire-server.local` lewat Wi-Fi yang sama, jadi server bisa pindah ke Mac lain atau Wi-Fi lain tanpa mengubah app dan board (lihat [Alamat untuk ESP32 dan app](#4-alamat-untuk-esp32-dan-app)). Notifikasi tetap sampai ke iPhone di mana saja (juga lewat data seluler), karena Mac yang menghubungi server Apple.

> Status: backend sudah lengkap dan teruji (lihat [Pengujian](#pengujian)). **Firmware dan app belum diubah.** Bagian [Kontrak untuk firmware](#kontrak-untuk-firmware-esp32-s3) dan [Kontrak untuk app](#kontrak-untuk-app-ios) menjelaskan apa yang nanti perlu ditambahkan di sana.

## Isi folder

```
BeforeFire-Backend/
├── cmd/beforefire-backend/main.go   titik awal program
├── internal/                        kode backend, dibagi per tugas
│   ├── alert/      memeriksa alert, menyusun isi notifikasi, mencegah duplikat
│   ├── apns/       mengirim notifikasi ke Apple (dan mode dry run)
│   ├── app/        menyambungkan semua bagian + integration test
│   ├── command/    perintah dari app, format tanda tangan, menunggu ack alat
│   ├── config/     membaca pengaturan dari .env
│   ├── device/     format ID alat, topic MQTT, status online/offline
│   ├── httpapi/    endpoint HTTP untuk app
│   ├── telemetry/  data sensor terakhir dan kunci baca (read key)
│   └── mqtt/       koneksi ke broker MQTT (membungkus library paho)
├── deploy/mosquitto/config/   mosquitto.conf dan acl (passwd dibuat oleh script)
├── scripts/
│   ├── mqtt-user.sh           tambah/hapus user MQTT (backend dan tiap alat)
│   ├── setup-server.sh        siapkan Mac: .env, password acak, user broker, daftar alat
│   ├── start-server.sh        nyalakan server dan nama beforefire-server.local
│   ├── stop-server.sh         hentikan server yang dijalankan dengan --background
│   ├── check-server.sh        cek server lewat nama beforefire-server.local
│   └── test-integration.sh    tes tanpa Docker (butuh go + mosquitto terpasang)
├── secrets/                   tempat AuthKey.p8 dari Apple (tidak ikut Git)
├── Dockerfile, docker-compose.yml, .env.example
```

Library pihak ketiga hanya satu: [`github.com/eclipse/paho.mqtt.golang`](https://github.com/eclipse/paho.mqtt.golang) v1.5.1 untuk MQTT (ikut menarik `gorilla/websocket`, `golang.org/x/net`, dan `golang.org/x/sync`). Library ini hanya ada di backend, tidak di app maupun firmware. APNs memakai fitur bawaan Go (HTTP/2 dan tanda tangan ES256).

## Menjalankan di Mac (Docker)

Satu file `docker-compose.yml` berisi semuanya: broker (`mosquitto`), backend, dan tes otomatis (profile `test`, hanya jalan kalau dipanggil).

### 1. Tes otomatis

```bash
cd ~/code/project/Final\ Challenge/BeforeFire-Backend
docker compose --profile test run --rm test
```

Hasil yang benar: semua baris diawali `ok`, lalu `--- PASS: TestBackendWithMosquitto` dengan 7 skenario. Tes ini memakai broker terpisah (`mosquitto-test`) dengan user tes, jadi tidak menyentuh user sungguhan. Bersihkan sesudahnya dengan `docker compose --profile test down`.

### 2. Menyalakan server

```bash
scripts/setup-server.sh DAPUR_A1B2C3 abcd2345  # sekali per Mac: serial dan PoP dari stiker
scripts/start-server.sh --build                # nyalakan Docker dan nama beforefire-server.local
```

`setup-server.sh` aman dijalankan berulang kali:
- Membuat `.env` dari `.env.example` kalau belum ada. `.env` yang sudah ada tidak ditimpa.
- Mengisi `MQTT_PASSWORD` dengan nilai acak kalau masih kosong atau `change-me`. Sebelum mengubah `.env`, file lama disalin ke `.env.backup`. Password yang sudah diisi tidak diganti.
- Membuat user `backend` di broker, atau memperbarui password-nya. Alat yang sudah terdaftar tetap ada.
- Mendaftarkan setiap pasangan serial dan PoP yang diberikan. Untuk beberapa alat: `scripts/setup-server.sh DAPUR_A1B2C3 abcd2345 DAPUR_D4E5F6 wxyz6789`.

Pengaturan APNs di `.env` tetap diisi manual (lihat langkah 5).

Biarkan jendela Terminal itu terbuka selama server dipakai, dan ketik perintah lain di tab baru. Kalau ingin Terminal langsung bisa dipakai lagi, jalankan di latar belakang:

```bash
scripts/start-server.sh --build --background   # Terminal langsung bebas, boleh ditutup
scripts/stop-server.sh                         # hapus nama dan matikan container
```

Di latar belakang, nama tetap aktif walaupun jendela Terminal ditutup, sampai `scripts/stop-server.sh` atau Mac di-restart. Lognya ada di `.server-name.log` (`tail -f .server-name.log`). Log backend bisa dilihat dengan `docker compose logs -f backend` (tunggu sampai muncul `mqtt connected`).

Cek dari Mac mana pun di Wi-Fi yang sama:

```bash
scripts/check-server.sh               # nama, Bonjour, HTTP, MQTT, dan daftar alat
scripts/check-server.sh DAPUR_A1B2C3  # juga cek bahwa alat ini online
```

Setiap baris menampilkan `OK` atau `FAIL` beserta saran perbaikan. Cara manual: `curl beforefire-server.local:8080/healthz` harus menjawab `{"mqttConnected":true}`.

Kalau muncul `port is already allocated`, port itu sudah dipakai program lain di Mac. Ganti `HTTP_PORT` (atau `MQTT_PORT`) di `.env`, misalnya `HTTP_PORT=8081`, lalu jalankan `scripts/start-server.sh` lagi. Setelah itu pakai port yang baru di `curl`, di app (`http://beforefire-server.local:8081`), dan di board (`@M`), karena alamat bawaan app dan board memakai port 8080 dan 1883.

### 3. Mencoba tanpa ESP32 dan app

Pura-pura menjadi ESP32 (memakai serial dan PoP yang didaftarkan di langkah 2):

```bash
docker compose exec mosquitto mosquitto_pub -u DAPUR_A1B2C3 -P abcd2345 -q 1 \
  -t beforefire/DAPUR_A1B2C3/alert \
  -m '{"alertId":"tes-1","condition":"alarm","readings":{"stoveTemperatureC":231},"pushTokens":["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]}'
```

Di log backend muncul `APNs dry run` dengan isi notifikasi. Kirim ulang perintah yang sama: backend mengabaikannya (`duplicate alert report ignored`).

```bash
# Status alat dari sisi app
docker compose exec mosquitto mosquitto_pub -u DAPUR_A1B2C3 -P abcd2345 -q 1 -r -t beforefire/DAPUR_A1B2C3/status -m online
curl localhost:8080/v1/devices/DAPUR_A1B2C3/status
```

Data sensor untuk layar Monitor (contoh ini memakai PoP `abcd2345`, jadi kuncinya sudah dihitung):

```bash
# Pura-pura ESP32 mengirim telemetry (retained)
docker compose exec mosquitto mosquitto_pub -u DAPUR_A1B2C3 -P abcd2345 -q 1 -r \
  -t beforefire/DAPUR_A1B2C3/telemetry \
  -m '{"condition":"normal","score":12.5,"readKeyHash":"6b3f6671d12855f3048da9f9f26b67077a7e6ef568512789204146eb902e1b6b","readings":{"stoveTemperatureC":80,"iaq":45,"presence":true,"roomTemperatureC":30,"roomHumidityRH":60,"pressureHPa":1013}}'

# Pura-pura app membaca data (dengan read key)
curl -H "Authorization: Bearer 6055c941c040745007ad78c6cad28078f0e92dd4aaf06598a5e8a7446fb2b876" \
  localhost:8080/v1/devices/DAPUR_A1B2C3/readings
```

Tanpa header `Authorization` atau dengan kunci lain, jawabannya `401 invalid_read_key`.

### 4. Alamat untuk ESP32 dan app

App dan firmware sudah memakai alamat bawaan berikut, jadi tidak perlu diketik:

- ESP32: `mqtt://beforefire-server.local:1883`
- App: `http://beforefire-server.local:8080`

`scripts/start-server.sh` memberi nama itu ke Mac yang menjalankannya, memakai `dns-sd` bawaan macOS. Nama laptop tidak diubah. Akibatnya:

- **Ganti Mac:** clone repo ini di Mac lain, jalankan `scripts/setup-server.sh <Serial> <PoP>`, lalu `scripts/start-server.sh --build`. App dan board tidak perlu diubah.
- **Ganti Wi-Fi:** script memindahkan nama ke IP baru dalam beberapa detik. App dan board tersambung lagi sendiri.
- Nama hanya aktif selama script berjalan. Ctrl+C menghapus nama; container tetap jalan sampai `docker compose down`. Dengan `--background`, nama aktif sampai `scripts/stop-server.sh` atau Mac di-restart.
- Hanya satu Mac di satu Wi-Fi yang bisa memakai nama itu. Script berhenti dengan pesan kalau nama sudah dipakai Mac lain.
- Script juga mengumumkan layanan Bonjour `_beforefire._tcp` (port HTTP, dengan TXT `http=` dan `mqtt=`). Cek dengan `dns-sd -B _beforefire._tcp`.

Kalau container sudah jalan (misalnya setelah Docker Desktop dibuka ulang), cukup `scripts/start-server.sh --name-only`.

**Wi-Fi yang memblokir nama `.local`** (sebagian Wi-Fi kampus atau kantor, atau router dengan *AP isolation*): pakai IP Mac sebagai cadangan. Cari dengan `ipconfig getifaddr en0` (contoh `192.168.1.10`), lalu ketik `http://192.168.1.10:8080` di layar Sensor Data app dan `@M mqtt://192.168.1.10:1883` di Serial Monitor board. Alamat ini harus diganti lagi kalau IP Mac berubah.

Supaya server tetap bisa dijangkau:
- Matikan sleep di System Settings → Energy.
- Biarkan Docker Desktop menyala. `restart: unless-stopped` membuat container menyala lagi setelah Docker Desktop dibuka ulang.

### 5. Mengirim notifikasi sungguhan ke iPhone

1. Di [Apple Developer](https://developer.apple.com/account/resources/authkeys/list) (perlu Apple Developer Program berbayar): **Keys → +**, centang **Apple Push Notifications service (APNs)**, unduh file `.p8` (hanya bisa diunduh satu kali), catat **Key ID**.
2. Simpan sebagai `secrets/AuthKey.p8`.
3. Di `.env`: `APNS_DRY_RUN=false` dan isi `APNS_KEY_ID`. `APNS_TEAM_ID` sudah terisi dari proyek Xcode (`L7P29889GM`).
4. `APNS_ENVIRONMENT=development` untuk app dari Xcode, `production` untuk TestFlight/App Store.
5. `docker compose up -d` untuk memuat ulang.

App juga perlu capability **Push Notifications**, dan push hanya bisa dites di iPhone fisik (bukan simulator).

### Live Activity saat BeforeFire tertutup

Docker Compose menyimpan pendaftaran ActivityKit dan sesi Alarm di volume `live-activity-state`. Jika backend dijalankan langsung, isi `LIVE_ACTIVITY_STATE_FILE` dengan path file pada direktori yang bisa ditulis dan dipertahankan setelah restart. Endpoint pendaftaran memakai read key sensor yang sudah ditautkan. File tersebut berisi token perangkat.

Untuk uji iPhone, gunakan konfigurasi APNs yang cocok dengan signing app. Buka BeforeFire sekali, izinkan notifikasi dan Live Activities, lalu tautkan sensor. Pakai kejadian uji. Cek bahwa push Alarm memulai Live Activity, ketukan membuka halaman Alarm, dan laporan Normal yang valid serta terbaru mengakhiri aktivitas. Warning 1, Warning 2, data kedaluwarsa, dan data sensor yang hilang mempertahankan sesi Alarm. Uji saat app di latar belakang dan saat ditutup dari App Switcher.

### Repeated Critical Alerts during Alarm

Set `ALARM_INTERRUPTION_LEVEL=critical`. Keep `LIVE_ACTIVITY_STATE_FILE` on persistent storage. Install the updated app and open it once after linking the sensor. The app registers its APNs token with its installation ID. The existing read key protects this request. Older app versions still receive the initial alert. They do not register for repeated alerts.

The server requests one Critical Alert every 30 seconds during an unresolved Alarm. Each request uses `BeforeFireCriticalSiren.caf` at volume `1.0`. The sound lasts 28.8 seconds. APNs delivery is best effort. The interval does not guarantee uninterrupted sound. The user can stop the current system sound with iPhone controls. A later alert can sound again.

A recent, complete Normal measurement after the Alarm stops the reminders and ends the Live Activity. Warning 1, Warning 2, stale readings, and connection loss keep the Alarm active. The server does not start another Live Activity for each reminder. It keeps the original Alarm event ID. A restart resumes the schedule without sending missed reminders in a burst.

The app sends snooze changes to `PUT /v1/devices/{deviceId}/live-activity-token`. A snooze body contains `installationId`, optional `eventId`, `snoozedUntil` (Unix seconds; `0` resumes), and `snoozeChangedAt` (Unix milliseconds). A newer change replaces an older change for that iPhone only. A snooze can last up to one hour. The server checks each phone deadline every second. It requests an alert when snooze ends, then repeats every 30 seconds from that request. It does not wait for another phone's schedule. APNs can still delay delivery. The app retries a pending change when it opens again. If the request cannot reach the server, the server can continue to send reminders. Requests already sent to APNs cannot be recalled.

Removing the installation clears its reminder token. APNs token rejection also stops reminders to that token. This feature does not change the physical alarm or its controls.

Verification: Test at least two reminder cycles on a locked iPhone. Snooze from the Live Activity, wait past the deadline, then send a fresh valid Normal reading. Check that reminders resume after snooze and stop after Normal. Check that a second linked iPhone keeps its own snooze state.

### Perintah lain

| Perintah | Fungsi |
|---|---|
| `scripts/setup-server.sh <Serial> <PoP>` | siapkan Mac (`.env`, password acak, user broker) dan daftarkan alat |
| `scripts/start-server.sh` | nyalakan server dan nama `beforefire-server.local` (Ctrl+C menghapus nama) |
| `scripts/check-server.sh [Serial]` | cek server lewat nama, seperti app dan board |
| `scripts/start-server.sh --build --background` | sama, tapi berjalan di latar belakang sehingga Terminal langsung bebas |
| `scripts/stop-server.sh` | hentikan server latar belakang: hapus nama dan matikan container (`--keep-docker` hanya menghapus nama) |
| `scripts/start-server.sh --name-only` | nyalakan nama saja untuk container yang sudah jalan |
| `docker compose down` | matikan server |
| `docker compose logs -f backend` | lihat log backend |
| `docker compose logs -f mosquitto` | lihat log broker |
| `scripts/mqtt-user.sh remove DAPUR_A1B2C3` | cabut akses satu alat |
| `scripts/start-server.sh --build` | jalankan ulang setelah kode berubah |

## Kontrak MQTT

Semua topic berbentuk `beforefire/<deviceId>/<jenis>`. `deviceId` adalah nama BLE dari firmware (`susunNamaBLE()`), contoh `DAPUR_A1B2C3`. Itu juga username MQTT alat. ACL membuat setiap alat hanya bisa memakai topic miliknya sendiri.

| Topic | Arah | QoS | Retain | Isi |
|---|---|---|---|---|
| `beforefire/<id>/alert` | alat → backend | 1 | tidak | laporan kondisi (JSON, maks 8 KB) |
| `beforefire/<id>/cmd` | backend → alat | 1 | tidak | perintah (JSON) |
| `beforefire/<id>/ack` | alat → backend | 1 | tidak | jawaban perintah (JSON) |
| `beforefire/<id>/status` | alat → backend | 1 | **ya** | `online` / `offline` (offline lewat MQTT Will) |
| `beforefire/<id>/telemetry` | alat → backend | 0 atau 1 | **ya** | data sensor terkini (JSON, maks 4 KB) |

## Kontrak untuk firmware (ESP32-S3)

Belum diterapkan di `A09Final-IoT`. Yang perlu ditambahkan di `Sensor.ino`:

**1. Koneksi**
- Broker: `mqtt://beforefire-server.local:1883` (atau `mqtt://<IP Mac>:1883`), username = `namaBLE`, password = PoP (prototipe) atau password terpisah di NVS (produksi).
- Tanpa TLS selama server ada di jaringan rumah. Tambahkan TLS sebelum broker bisa diakses dari internet.
- Will: topic `beforefire/<id>/status`, payload `offline`, QoS 1, retain.
- Setelah tersambung: publish `online` ke topic yang sama, QoS 1, retain.
- Subscribe `beforefire/<id>/cmd` QoS 1.
- Sinkronkan jam lewat NTP (`configTime`) untuk `issuedAt` dan pemeriksaan perintah.

**2. Mengirim alert** saat `evaluateFuzzy()` berganti kondisi:

```json
{
  "alertId": "b3-000127",
  "condition": "alarm",
  "score": 87.5,
  "issuedAt": 1730700000,
  "readings": {
    "stoveTemperatureC": 231.6,
    "presence": false,
    "absenceDurationS": 420,
    "iaq": 120,
    "roomTemperatureC": 31.2,
    "roomHumidityRH": 55
  },
  "pushTokens": ["<token HP 1>", "<token HP 2>"]
}
```

- `alertId`: unik per kejadian, 1–64 karakter `A-Z a-z 0-9 _ -`. Dipakai juga sebagai `apns-collapse-id`.
- `condition`: `normal`, `warning1`, `warning2`, atau `alarm` (sama dengan `enum class Condition`).
- `issuedAt`: Unix detik. Boleh `0` atau tidak dikirim kalau jam belum sinkron (backend memakai waktu terima).
- `readings`: semuanya opsional.
- `pushTokens`: 1–10 token dari NVS. Alert tanpa token ditolak.

Isi notifikasi per kondisi:

| condition | Judul | Interruption level | Suara |
|---|---|---|---|
| `alarm` | Check your kitchen now | `ALARM_INTERRUPTION_LEVEL` (default time-sensitive) | ya |
| `warning2` | Kitchen warning | time-sensitive | ya |
| `warning1` | Check your kitchen | active | ya |
| `normal` | Kitchen readings are normal | passive | tidak |

Kalau `stoveTemperatureC` ada, isi notifikasi ditambah "Stove: 232 °C." (kecuali `normal`).

**3. Mengirim telemetry** (data untuk layar Monitor) setiap 2 detik, dan saat itu juga kalau level bahaya atau keberadaan orang berubah, ke `beforefire/<id>/telemetry` sebagai pesan **retained**:

```json
{
  "condition": "normal",
  "score": 12.5,
  "measuredAt": 1730700000,
  "readKeyHash": "<64 hex, dihitung sekali saat boot>",
  "readings": {
    "stoveTemperatureC": 80,
    "iaq": 45,
    "iaqAccuracy": 3,
    "eco2Ppm": 600,
    "presence": true,
    "absenceDurationS": 0,
    "roomTemperatureC": 30,
    "roomHumidityRH": 60,
    "pressureHPa": 1013.2
  },
  "unavailableSensors": ["thermal"]
}
```

- Sumber nilai di `Sensor.ino`: `mlxKompor` → `stoveTemperatureC`, `bsecIaq`/`bsecAkurasi` → `iaq`/`iaqAccuracy`, `bsecCo2` → `eco2Ppm`, `presence` → `presence`, `bsecSuhu`/`bsecLembap`/`bsecTekanan` → `roomTemperatureC`/`roomHumidityRH`/`pressureHPa`.
- `condition` dan `score` dari `evaluateFuzzy()` (opsional sampai fungsi itu dipanggil).
- Sertakan `measuredAt` dari jam NTP. Tanpa itu, backend memakai waktu terima, sehingga setelah backend restart data retained yang lama terlihat baru (`isStale: false`).
- Semua nilai di `readings` opsional. Jangan kirim nilai sensor yang sedang rusak. Tulis nama sensornya di `unavailableSensors` (`bme`, `radar`, `thermal`).
- `readKeyHash` = `hex(SHA-256(readKey))`, dengan `readKey` dihitung dari PoP (lihat [Kunci baca data sensor](#kunci-baca-data-sensor)). Hitung ulang kalau PoP diganti (`@R`).

**4. Menerima perintah** di `beforefire/<id>/cmd`:

```json
{"action":"silence","requestId":"3f1c2b7e-...","issuedAt":1730700000,"signature":"c354...","origin":"app"}
```

| action | Tindakan alat |
|---|---|
| `silence` | matikan alarm ESP32-C3 lewat Wi-Fi lokal |
| `register_push_token` | simpan `pushToken` di NVS (abaikan kalau sudah ada, batasi jumlahnya) |
| `unregister_push_token` | hapus `pushToken` dari NVS |

Aturan pemeriksaan:
- `origin: "app"`: **wajib** periksa tanda tangan (lihat di bawah), `issuedAt` paling jauh ±5 menit dari jam alat, dan `requestId` belum pernah dipakai (simpan sekitar 16 ID terakhir).
- `origin: "backend"`: tanpa tanda tangan, **hanya** boleh untuk `unregister_push_token`. Backend mengirimnya saat APNs menjawab token sudah tidak terdaftar (HTTP 410), dan saat APNs menolak token dengan `BadDeviceToken` sedangkan token lain dari alert yang sama terkirim. Contohnya token dari build Xcode (sandbox) saat backend memakai `production` untuk TestFlight. Kalau semua token ditolak `BadDeviceToken`, kemungkinan `APNS_ENVIRONMENT` salah, jadi tidak ada token yang dihapus. Hanya user `backend` yang boleh publish ke topic `cmd` (ACL).

**5. Menjawab** di `beforefire/<id>/ack`:

```json
{"requestId":"3f1c2b7e-...","result":"done"}
{"requestId":"3f1c2b7e-...","result":"rejected","reason":"bad_signature"}
```

Alasan yang disarankan: `bad_signature`, `expired`, `replayed`, `unknown_action`, `alarm_unreachable`, `token_storage_full`.

### Tanda tangan perintah

Backend tidak menyimpan PoP, jadi app menandatangani perintah dengan PoP dari stiker dan alat memeriksanya dengan PoP di NVS.

```
teks      = "beforefire-cmd-v1\n<deviceId>\n<action>\n<requestId>\n<issuedAt>\n<pushToken>"
signature = hex_lowercase( HMAC-SHA256( key = PoP, message = teks ) )
```

`pushToken` kosong untuk `silence`. Tidak ada baris baru di akhir.

Contoh untuk menguji implementasi app dan firmware:

| | |
|---|---|
| PoP | `abcd2345` |
| deviceId | `DAPUR_A1B2C3` |
| action | `silence` |
| requestId | `3f1c2b7e-0000-4000-8000-000000000001` |
| issuedAt | `1730700000` |
| signature | `c3543e3f62513b35540d7f523867b05129d7281707750f6f043e74728d9f499c` |

Di firmware bisa memakai `mbedtls_md_hmac()` dengan `MBEDTLS_MD_SHA256`, dan bandingkan hasilnya dengan perbandingan waktu-tetap.

### Kunci baca data sensor

Backend tidak menyimpan PoP, jadi data sensor dilindungi dengan *read key* yang diturunkan dari PoP:

```
readKey     = hex_lowercase( HMAC-SHA256( key = PoP, message = "beforefire-read-v1\n" + deviceId ) )
readKeyHash = hex_lowercase( SHA-256( readKey ) )      ← dikirim firmware di setiap telemetry
```

App mengirim `readKey`. Backend menghitung SHA-256-nya dan mencocokkannya dengan `readKeyHash` dari alat. Backend tidak pernah tahu PoP maupun `readKey` sebelum app mengirimnya.

| | |
|---|---|
| PoP | `abcd2345` |
| deviceId | `DAPUR_A1B2C3` |
| readKey | `6055c941c040745007ad78c6cad28078f0e92dd4aaf06598a5e8a7446fb2b876` |
| readKeyHash | `6b3f6671d12855f3048da9f9f26b67077a7e6ef568512789204146eb902e1b6b` |

## Kontrak untuk app (iOS)

Belum diterapkan di `BeforeFire`. Alamat backend: `http://beforefire-server.local:8080` (atau `http://<IP Mac>:8080`).

iOS memblokir `http://` biasa. Tambahkan di Info.plist app:
- `NSAppTransportSecurity` → `NSAllowsLocalNetworking` = `YES` (izin koneksi ke jaringan lokal tanpa HTTPS).
- `NSLocalNetworkUsageDescription` (iOS meminta izin akses jaringan lokal ke user).

Endpoint:

| Method | Path | Fungsi |
|---|---|---|
| `GET` | `/healthz` | `200 {"mqttConnected":true}` atau `503` |
| `GET` | `/v1/devices` | `{"devices":[{"deviceId":"…","connection":"online\|offline\|unknown","updatedAt":"…"}]}`, semua alat yang pernah mengirim status sejak backend menyala. App memakainya untuk menemukan sensor yang sudah punya Wi-Fi. Hanya berisi ID dan status, tanpa data sensor. |
| `GET` | `/v1/devices/{deviceId}/status` | `{"deviceId":"…","connection":"online\|offline\|unknown","updatedAt":"…"}` |
| `GET` | `/v1/devices/{deviceId}/readings` | data sensor terkini. Header: `Authorization: Bearer <readKey>` |
| `POST` | `/v1/devices/{deviceId}/commands` | kirim perintah, tunggu jawaban alat (maks `COMMAND_ANSWER_TIMEOUT`) |

Body `POST /commands`:

```json
{"action":"register_push_token","requestId":"<UUID>","issuedAt":1730700000,"pushToken":"<hex>","signature":"<hex>"}
```

Jawaban:

| HTTP | `error` | Arti untuk user |
|---|---|---|
| 200 | – | alat sudah menjalankan perintah (`{"result":"done"}`) |
| 400 | `invalid_request` | format salah, atau jam HP tidak tepat |
| 404 | `unknown_device_id` | serial bukan format `DAPUR_xxxxxx` |
| 409 | `duplicate_request` | `requestId` yang sama sedang diproses |
| 422 | `command_rejected` | alat menolak (`detail` berisi alasan, misalnya `bad_signature` = PoP salah) |
| 503 | `device_offline` / `broker_unavailable` | alat atau server sedang tidak tersambung |
| 504 | `device_no_answer` | alat tidak menjawab tepat waktu |

**Data sensor untuk layar Monitor** (`GET /readings`):

```json
{
  "deviceId": "DAPUR_A1B2C3",
  "condition": "normal",
  "score": 12.5,
  "readings": {"stoveTemperatureC": 80, "iaq": 45, "presence": true, "roomTemperatureC": 30, "roomHumidityRH": 60, "pressureHPa": 1013.2},
  "unavailableSensors": [],
  "measuredAt": "2026-11-05T04:30:00Z",
  "ageSeconds": 3,
  "isStale": false,
  "connection": "online"
}
```

| HTTP | `error` | Arti |
|---|---|---|
| 200 | – | data tersedia |
| 401 | `invalid_read_key` | kunci salah atau tidak dikirim (PoP salah, atau alat sudah membuat PoP baru) |
| 404 | `no_readings` / `unknown_device_id` | alat belum pernah mengirim telemetry / serial salah |

- `isStale: true` artinya tidak ada data baru selama `TELEMETRY_STALE_AFTER` (default 10 detik). Tampilkan sebagai data lama, bukan kondisi normal.
- Nilai yang tidak ada di `readings` atau sensornya tercantum di `unavailableSensors` ditampilkan sebagai "tidak tersedia".
- Ambil ulang setiap beberapa detik saat layar Monitor terbuka.

Pemetaan ke kartu `MonitorReading.Kind`:

| Kartu | Field |
|---|---|
| `stoveHeat` | `stoveTemperatureC` |
| `airQuality` | `iaq` (misalnya ≤ 50 Excellent, ≤ 100 Good, sesuai `iaqKeHomeKit()` di firmware) |
| `presence` | `presence` |
| `roomTemperature` | `roomTemperatureC` |
| `roomHumidity` | `roomHumidityRH` |
| `areaPressure` | `pressureHPa` |

Usulan status utama: `normal` → Safe, `warning1`/`warning2` → Unsafe, `alarm` → Dangerous (perlu dikonfirmasi pemilik desain).

Menghitung read key di app (CryptoKit):

```swift
let message = Data("beforefire-read-v1\n\(deviceID)".utf8)
let code = HMAC<SHA256>.authenticationCode(for: message, using: SymmetricKey(data: Data(pop.utf8)))
let readKey = code.map { String(format: "%02x", $0) }.joined()
// request.setValue("Bearer \(readKey)", forHTTPHeaderField: "Authorization")
```

Alur pendaftaran notifikasi:
1. App mendapat device token APNs, ubah ke hex huruf kecil.
2. App mengirim `register_push_token` yang sudah ditandatangani dengan PoP dari stiker (PoP yang sama yang dipakai saat provisioning BLE).
3. Alat menyimpan token, lalu menyertakannya di setiap alert.

Contoh tanda tangan dengan CryptoKit:

```swift
import CryptoKit

let text = ["beforefire-cmd-v1", deviceID, action, requestID, String(issuedAt), pushToken]
    .joined(separator: "\n")
let code = HMAC<SHA256>.authenticationCode(for: Data(text.utf8), using: SymmetricKey(data: Data(pop.utf8)))
let signature = code.map { String(format: "%02x", $0) }.joined()
```

Payload notifikasi berisi `userInfo["beforefire"]` = `{deviceId, alertId, condition, score, issuedAt}` untuk membuka detail alert. Notifikasi `time-sensitive` butuh capability **Time Sensitive Notifications** di target app. `critical` butuh entitlement **Critical Alerts** yang harus disetujui Apple.

## Pengaturan (`.env`)

Semua variabel dijelaskan di `.env.example`. Yang wajib: `MQTT_PASSWORD`, dan (kalau `APNS_DRY_RUN=false`) `APNS_KEY_ID` serta file `secrets/AuthKey.p8`. Backend menolak start kalau ada nilai yang salah dan menyebutkan semuanya sekaligus.

## Pengujian

Yang sudah diuji di lingkungan pengembangan:

- `go test -race ./...`: unit test untuk semua paket (JWT ES256 diverifikasi dengan public key, header dan retry APNs terhadap server HTTP/2 tiruan, validasi alert dan perintah, pemetaan error HTTP, konfigurasi).
- Integration test dengan **Mosquitto 2.0.18** dan file ACL asli, juga dengan script broker tes dari `docker-compose.yml`: alert sampai satu kali walaupun dikirim dua kali, ACL menolak alert atas nama alat lain, perintah bertanda tangan benar dijawab `done`, tanda tangan salah ditolak alat, token 410 dikirim balik ke alat, telemetry hanya bisa dibaca dengan read key yang benar dan alat tidak bisa mengirim telemetry atas nama alat lain, status offline membuat perintah langsung gagal.
- Uji manual binary: backend menunggu kalau broker belum menyala, tersambung ulang dan subscribe ulang otomatis setelah broker mati lalu hidup, MQTT Will (`offline`) muncul setelah alat terputus mendadak, `-healthcheck` bekerja.
- `scripts/mqtt-user.sh` (init, add, remove, ID salah) dengan perintah Docker tiruan.
- Nilai contoh tanda tangan dicek dengan `openssl dgst -sha256 -hmac`.
- `docker compose config` valid, termasuk tanpa file `.env`.

Yang **belum** diuji:

- `docker build` dan `docker compose up` sungguhan (image tidak bisa diunduh di lingkungan pengembangan). Ini akan pertama kali berjalan di Mac Anda.
- Pengiriman ke server APNs Apple yang asli (perlu file `.p8`).
- Koneksi dari ESP32 dan app sungguhan (firmware dan app belum diubah).
- Checksum di `go.sum` untuk `golang.org/x/net` dan `golang.org/x/sync` dibuat dari mirror GitHub karena `proxy.golang.org` diblokir di lingkungan pengembangan. Kalau `docker build` melaporkan *checksum mismatch*, hapus `go.sum` lalu jalankan `docker run --rm -v "$PWD":/src -w /src golang:1.24 go mod tidy`.

## Keterbatasan dan TODO

- **Arsitektur berbeda dari PROJECT_SPEC.md.** Spesifikasi menyebut jalur notifikasi "HomeKit webhook → APNs". Backend ini memakai MQTT sesuai diskusi terakhir. HomeKit lewat HomeSpan tetap dipakai untuk Apple Home dan tidak terpengaruh. Perlu diputuskan apakah spesifikasi diperbarui.
- **Hanya untuk jaringan rumah.** MQTT (1883) dan HTTP (8080) tanpa enkripsi. Perintah dari app hanya bisa dikirim saat iPhone satu Wi-Fi dengan Mac. Untuk akses dari luar rumah, tambahkan tunnel (misalnya Cloudflare Tunnel) beserta HTTPS dan TLS.
- Belum ada rate limit di endpoint HTTP.
- `GET /status` tidak butuh tanda tangan (hanya membuka status online/offline).
- Push untuk Live Activity (`apns-push-type: liveactivity`) belum dibuat.
- Prototipe memakai PoP sebagai password MQTT. Untuk produksi, buat password terpisah per board saat flashing (`scripts/mqtt-user.sh add <ID>` tanpa password akan membuatkannya).


## WidgetKit status updates

The iOS 26+ Kitchen Status widget can refresh without opening BeforeFire.
The widget registers a separate WidgetKit token. It does not use the APNs alert token or the ActivityKit token.

- `PUT /v1/devices/{deviceId}/widget-token` requires the sensor read key in `Authorization: Bearer <readKey>`.
- Register: `{"installationId":"<UUID>","pushToken":"<WidgetKit hex token>"}`.
- Remove: `{"installationId":"<UUID>","remove":true}`. The app retains failed removals and retries them.
- The backend observes the latest telemetry and sensor connection. A change to the condition, rule, validity, sensor availability, or freshness queues a widget refresh. Numeric readings and absence-duration increments do not each send a push.
- Changes are coalesced for up to 10 seconds per installation. Failed sends retry after 10 seconds. A 410 response removes the token. Configuration errors do not remove it.
- Requests use `apns-push-type: widgets`, `apns-topic: <APNS_TOPIC>.push-type.widgets`, priority 5, and `{"aps":{"content-changed":true}}`. They contain no alert text or sound.
- iOS budgets these refreshes. Delivery does not guarantee an immediate widget update. Periodic timelines remain enabled. Critical Alerts and Live Activities keep their separate paths.

`WIDGET_PUSH_STATE_FILE` stores subscriptions. Its default is `data/widget-push.json`. Docker uses `/state/widget-push.json` in the existing persistent state volume. Protect this file because it contains device tokens. A store error disables widget registration and is logged; it does not stop alarm delivery.

Deploy this backend and install the updated app/widget together. Open the app once to share the sensor link. Add Kitchen Status to the Home Screen. Confirm that the widget token endpoint returns 204, then change the sensor condition with BeforeFire in the background. Test sandbox for Xcode builds and production for TestFlight. These steps require physical-device validation.

Reference: https://developer.apple.com/documentation/widgetkit/updating-widgets-with-widgetkit-push-notifications


### Critical sound policy (2026-11-19)

Warning 1 (Unsafe) and Warning 2 (Dangerous) use Critical Alerts with the short system notification sound: `{"critical":1,"name":"default","volume":1}`. Alarm keeps `BeforeFireCriticalSiren.caf` and its existing reminder/snooze behavior. Normal is silent. Warning notifications do not start the alarm reminder loop or an Alarm Live Activity. These changes require the app's Critical Alerts entitlement and the user's Critical Alerts permission. The iPhone test confirms delivery separately from payload tests.


### Compact notification copy (2026-11-19)

APNs bodies use at most two short text lines. They do not append sensor temperatures or rule descriptions. The app keeps detailed guidance in Monitor and the Alarm screen. Alarm repeats and the ActivityKit start alert use the same short Alarm body. System font and display settings can still affect wrapping.

- Unsafe: `Check your kitchen.` / `Check the stove and nearby area.`
- Dangerous: `Check the stove NOW.` / `Turn it off if safe.`
- Alarm: `Turn stove off NOW if safe.` / `If not, call someone nearby.`
- Normal: `Readings are back to normal.`
