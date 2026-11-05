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

Server berjalan di Mac. ESP32 dan app tersambung ke IP Mac lewat Wi-Fi rumah. Notifikasi tetap sampai ke iPhone di mana saja (juga lewat data seluler), karena Mac yang menghubungi server Apple.

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
cp .env.example .env
# Buka .env, ganti MQTT_PASSWORD dengan nilai acak (contoh: openssl rand -base64 24)

scripts/mqtt-user.sh init                      # buat user "backend" di broker
scripts/mqtt-user.sh add DAPUR_A1B2C3 abcd2345 # daftarkan alat: serial dan PoP dari stiker
docker compose up -d --build
docker compose logs -f backend                 # tunggu sampai muncul "mqtt connected"
```

Cek: `curl localhost:8080/healthz` harus menjawab `{"mqttConnected":true}`.

Kalau muncul `port is already allocated`, port itu sudah dipakai program lain di Mac. Ganti `HTTP_PORT` (atau `MQTT_PORT`) di `.env`, misalnya `HTTP_PORT=8081`, lalu jalankan `docker compose up -d` lagi. Setelah itu pakai port yang baru di `curl` dan di app.

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

Cari IP Mac: `ipconfig getifaddr en0` (contoh `192.168.1.10`).

- ESP32: `mqtt://192.168.1.10:1883`
- App: `http://192.168.1.10:8080`

Supaya alamat itu tidak berubah:
- Buat *DHCP reservation* untuk Mac di router.
- Matikan sleep di System Settings → Energy.
- Biarkan Docker Desktop menyala. `restart: unless-stopped` membuat container menyala lagi setelah Docker Desktop dibuka ulang.

### 5. Mengirim notifikasi sungguhan ke iPhone

1. Di [Apple Developer](https://developer.apple.com/account/resources/authkeys/list) (perlu Apple Developer Program berbayar): **Keys → +**, centang **Apple Push Notifications service (APNs)**, unduh file `.p8` (hanya bisa diunduh satu kali), catat **Key ID**.
2. Simpan sebagai `secrets/AuthKey.p8`.
3. Di `.env`: `APNS_DRY_RUN=false` dan isi `APNS_KEY_ID`. `APNS_TEAM_ID` sudah terisi dari proyek Xcode (`L7P29889GM`).
4. `APNS_ENVIRONMENT=development` untuk app dari Xcode, `production` untuk TestFlight/App Store.
5. `docker compose up -d` untuk memuat ulang.

App juga perlu capability **Push Notifications**, dan push hanya bisa dites di iPhone fisik (bukan simulator).

### Perintah lain

| Perintah | Fungsi |
|---|---|
| `docker compose down` | matikan server |
| `docker compose logs -f backend` | lihat log backend |
| `docker compose logs -f mosquitto` | lihat log broker |
| `scripts/mqtt-user.sh remove DAPUR_A1B2C3` | cabut akses satu alat |
| `docker compose up -d --build` | jalankan ulang setelah kode berubah |

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
- Broker: `mqtt://<IP Mac>:1883`, username = `namaBLE`, password = PoP (prototipe) atau password terpisah di NVS (produksi).
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
- `origin: "backend"`: tanpa tanda tangan, **hanya** boleh untuk `unregister_push_token`. Backend mengirimnya saat APNs menjawab token sudah tidak terdaftar (HTTP 410). Hanya user `backend` yang boleh publish ke topic `cmd` (ACL).

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

Belum diterapkan di `BeforeFire`. Alamat backend: `http://<IP Mac>:8080`.

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
