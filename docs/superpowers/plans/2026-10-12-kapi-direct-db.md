# Integrasi Langsung ADMS → Database KAPI: Rencana Implementasi

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Server ADMS berbasis library ini menulis data alat langsung ke database KAPI (MariaDB) dan mengirim command dari antrian KAPI ke alat, menggantikan Push Logger lama dan hop REST `KAPI_API_PUSHLOGGER`.

**Architecture:** Library `zkadms` (root, tanpa dependensi) mendapat parser dan callback netral vendor untuk `OPERLOG`, `BIODATA`, dan `options`. Modul Go terpisah `kapi/` (satu dependensi: `go-sql-driver/mysql`) berisi seluruh logika KAPI di balik antarmuka kecil (ports) yang bisa diganti `fakestore` pada tes; implementasi MySQL ada di paket `store`.

**Tech Stack:** Go 1.26, `database/sql`, `github.com/go-sql-driver/mysql`, MariaDB 10.3.

**Spec:** `docs/superpowers/specs/2026-10-12-kapi-direct-db-design.md` (wajib dibaca bersama rencana ini).

## Global Constraints

- Root `go.mod` tetap tanpa dependensi. Modul `kapi/`: path `github.com/s0x90/zkteco-adms/kapi`, `replace github.com/s0x90/zkteco-adms => ../`, satu-satunya dependensi eksternal `github.com/go-sql-driver/mysql`.
- Gaya root: ejaan US English, `gofumpt`/`goimports`, tanpa `init()`, setiap fungsi/metode publik baru di root wajib dipanggil tes (gerbang `deadcode`), `make test` dan `make lint` hijau. Aturan sama dipakai di `kapi/`.
- Tidak pernah mencatat isi template, foto, password, atau nomor kartu di log; log hanya SN, PIN, jenis, ukuran. DSN hanya dari env `KAPI_DSN`.
- Tes integrasi hanya ke MariaDB 10.3 sementara lewat `KAPI_TEST_DSN`, dengan build tag `integration`; harness menolak nama database yang mengandung `prod`. **Dilarang menulis ke `kapi_test_prod2`** tanpa izin tegas pemilik proyek.
- `flag` absensi: `Status` bila 0..5, selain itu (255 atau di luar rentang) kode verifikasi. `presence_date` = jam dinding alat dipotong ke menit. Konstanta: `approved_status_id` = `282bdd5f-61d9-4f7b-bcc1-42d109243f3b`; `trx_a_presence`: `type_device='Device'`, `created_by='Logger'`; `trx_a_process_attendance`: `created_by='Auto Process'`, `updated_by='Push Logger'`, `period` format `Jan 2006` (mis. `Oct 2026`); `mst_a_device_cmd_log` hasil kita: `type_log='Auto Update'`, `created_by='Auto Update'`, `trx_id=0`.
- Status alat: 0 `Not Connect`, 1 `Connect`, 2 `Process Sync (N menunggu)`. Command terkirim tanpa balasan >10 menit: dikembalikan ke antrian maksimal 3 kali, lalu `return_value='Timeout'`.
- Error sementara diulang di tempat lewat `RetryPolicy.Do` (callback tunggal terblokir → library membalas 503 → alat mengirim ulang). Error permanen dicatat (SN, PIN, jenis; tanpa isi biometrik) lalu data itu dilewati agar tidak menahan yang lain.
- Trailer commit: `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>` dan `Claude-Session: https://claude.ai/code/session_01DZV4sxoB3vhwFxmGnCyuho`.

## Review Focus

1. PIN dari alat berbentuk `0042`, `0`, negatif, non-angka, atau terlalu besar → `0042` jadi 42; sisanya ditolak tanpa panik dan tanpa menulis (Task 5, 8).
2. Baris `OPERLOG`/`BIODATA` rusak (tanpa PIN/Type, CRLF) dan foto >64 KB → baris rusak dilewati, konten besar utuh (Task 1, 2).
3. Database gagal sementara di tengah rombongan ATTLOG → diulang di tempat, hasil akhir tepat satu baris per absen, tidak ada yang hilang (Task 6, 8).
4. Command berisi CR/LF atau diklaim dua kali → ditandai `Invalid` tanpa diulang; satu klaim = satu pengiriman (Task 12).
5. `timezone` rusak atau alat belum pernah mengirim `options` → jatuh ke UTC dengan peringatan; penyebaran template baru dilewati, tidak pernah menebak (Task 7, 11).

---

## Tahap 0 — Library

### Task 1: Parser `OPERLOG` (user dan foto)

**Files:**
- Modify: `types.go` (tipe baru), `parser.go` (fungsi baru), `parser_test.go`

**Interfaces:**
- Produces:
  - `type UserPhoto struct { PIN, FileName, Kind string; Type, Size int; Content string }` (`Kind` = `"USERPIC"` atau `"BIOPHOTO"`; `Content` base64 apa adanya)
  - `type OperLogBatch struct { Users []UserRecord; Photos []UserPhoto }`
  - `func (s *ADMSServer) parseOperLog(data, serialNumber string) OperLogBatch`

- [ ] **Step 1: Tulis tes gagal** di `parser_test.go` (pola `parseUserRecords` yang ada):
  - `TestParseOperLog_User`: baris `"USER PIN=3\tName=ff\tPri=0\tPasswd=\tCard=\tGrp=1\tTZ=0000000100000000\tVerify=-1"` → `Users[0] == UserRecord{PIN:"3", Name:"ff"}`.
  - `TestParseOperLog_UserCredentials`: `"USER PIN=9\tName=Zed\tPri=14\tPasswd=123456\tCard=2252991"` → `Privilege==14`, `Password=="123456"`, `Card=="2252991"`.
  - `TestParseOperLog_Photos`: `"USERPIC PIN=3\tFileName=3.jpg\tSize=8\tContent=QUJDRA=="` dan `"BIOPHOTO PIN=3\tFileName=3.jpg\tType=9\tSize=8\tContent=QUJDRA=="` → dua `UserPhoto` dengan `Kind`, `Type` (0 untuk USERPIC, 9 untuk BIOPHOTO), `Content=="QUJDRA=="`.
  - `TestParseOperLog_LargeContentKept`: `Content` 70.000 karakter (`strings.Repeat("A", 70000)`) utuh, `Size` mengikuti baris.
  - `TestParseOperLog_IgnoresOplogSkipsMalformedHandlesCRLF`: `"OPLOG 4\t0\t2026-10-12 13:30:44\t0"` diabaikan; `"USER Name=x"` (tanpa PIN) dan `"USERPIC FileName=a.jpg"` dilewati; akhiran `\r\n` tidak masuk ke nilai.
- [ ] **Step 2: Jalankan** `go test -run TestParseOperLog ./...` → FAIL (`parseOperLog` belum ada).
- [ ] **Step 3: Implementasi** `parseOperLog`: pisah per baris, kata pertama sebelum spasi menentukan jenis (`USER`, `USERPIC`, `BIOPHOTO`; lainnya diabaikan); sisanya dipecah per tab dan `strings.Cut` pada `=` pertama (agar `==` base64 utuh). `Pri`→`Privilege`, `Passwd`→`Password`. Baris tanpa `PIN` dilewati dengan satu `Warn` yang memuat hitungan, bukan isi.
- [ ] **Step 4: Jalankan** `go test -race ./...` → PASS.
- [ ] **Step 5: Commit** `git add types.go parser.go parser_test.go && git commit -m "Parse OPERLOG user and photo lines"`.

### Task 2: Parser `BIODATA`

**Files:**
- Modify: `types.go`, `parser.go`, `parser_test.go`

**Interfaces:**
- Produces: `type BioTemplate struct { PIN string; No, Index, Valid, Duress, Type, MajorVer, MinorVer, Format int; Data string }` dan `func (s *ADMSServer) parseBioTemplates(data, serialNumber string) []BioTemplate`.

- [ ] **Step 1: Tes gagal:**
  - `TestParseBioTemplates_FaceAndPalm`: `"BIODATA Pin=3\tNo=0\tIndex=0\tValid=1\tDuress=0\tType=9\tMajorVer=36\tMinorVer=1\tFormat=0\tTmp=QUJD=="` → `{PIN:"3", Type:9, MajorVer:36, MinorVer:1, Valid:1, Data:"QUJD=="}`; baris kedua `Type=8\tMajorVer=20` → `Type==8`, `MajorVer==20`.
  - `TestParseBioTemplates_PinKeyCaseInsensitive`: `PIN=` dan `Pin=` sama-sama diterima.
  - `TestParseBioTemplates_SkipsMissingPinOrType`: baris tanpa PIN atau tanpa `Type` dilewati; baris valid sesudahnya tetap terbaca.
- [ ] **Step 2:** `go test -run TestParseBioTemplates ./...` → FAIL.
- [ ] **Step 3: Implementasi** `parseBioTemplates` dengan pola Task 1; kunci dicocokkan `strings.EqualFold`; angka tak valid → 0 kecuali `Type` yang wajib ada.
- [ ] **Step 4:** `go test -race ./...` → PASS.
- [ ] **Step 5: Commit** `"Parse BIODATA template lines"`.

### Task 3: Callback, routing, parsing `options`, dokumentasi

**Files:**
- Modify: `types.go` (konstanta `tableBIODATA = "BIODATA"`, `tableOptions = "options"`), `adms.go` (field `onOperLog`, `onBioData`, fungsi dispatch), `options.go`, `handler.go` (`HandleCData`), `handler_test.go`, `options_test.go`, `datasheet/ADMS.md`

**Interfaces:**
- Consumes: `parseOperLog`, `parseBioTemplates` (Task 1–2).
- Produces:
  - `func WithOnOperLog(fn func(ctx context.Context, sn string, batch OperLogBatch)) Option`
  - `func WithOnBioData(fn func(ctx context.Context, sn string, templates []BioTemplate)) Option`
  - `table=options`: dikirim ke `WithOnDeviceInfo` sebagai map yang dipecah per koma, awalan `~` dibuang; CRLF/LF juga dianggap pemisah.

- [ ] **Step 1: Tes gagal** (gaya `TestHandleCData_USERINFO`: `httptest.NewRequest` → `server.HandleCData`, tunggu channel dengan `time.After`):
  - `TestHandleCData_OperLog_DispatchesBatch`: body berisi satu `USER`, satu `USERPIC`, satu `OPLOG` → callback menerima 1 user, 1 foto; respons 200 `"OK"`.
  - `TestHandleCData_OperLog_CallbackQueueFull` dan `TestHandleCData_BioData_CallbackQueueFull`: meniru `TestHandleCData_USERINFO_CallbackQueueFull` → 503.
  - `TestHandleCData_BioData_Dispatches`: dua baris BIODATA → callback `[]BioTemplate` panjang 2, respons 200 `"OK"`.
  - `TestHandleCData_BioData_NoCallback_DoesNotCallDeviceInfo`: tanpa `WithOnBioData`, 200 `"OK"` dan `WithOnDeviceInfo` **tidak** dipanggil (regresi: sebelumnya BIODATA jatuh ke cabang info).
  - `TestHandleCData_Options_ParsesCommaSeparated`: body `"~DeviceName=FA01,MAC=00:11,FWVersion=ZAM230-NF50VA-Ver1.3.7,IPAddress=192.168.1.99"` → map berisi 4 kunci, `info["DeviceName"]=="FA01"`, `info["FWVersion"]=="ZAM230-NF50VA-Ver1.3.7"`; respons tetap membawa command tertunda seperti cabang lama.
  - `TestWithOnOperLog_And_WithOnBioData_SetCallbacks` di `options_test.go`.
  - Pastikan `TestHandleCData_OperationLog` yang ada tetap lulus tanpa diubah.
- [ ] **Step 2:** `go test -run 'TestHandleCData_(OperLog|BioData|Options)|TestWithOn' ./...` → FAIL.
- [ ] **Step 3: Implementasi:** tambah `case tableOPERLOG` (baca body, `parseOperLog`, dispatch lewat `dispatchCallback`, 503 bila `false`, selain itu 200 `OK`; tanpa callback tetap ack), `case tableBIODATA` (serupa), `case tableOptions` (parse dengan `strings.NewReplacer("\r\n", ",", "\n", ",")` lalu `parseKVPairs(body, ",", trimTildePrefix)`, dispatch ke `dispatchDeviceInfo`, lalu `writeCommandsOrOK`). Pola dispatch sama dengan `dispatchQueryUsers`.
- [ ] **Step 4: Dokumentasi:** tambah di `datasheet/ADMS.md` bagian "ZAM230-NF50VA real device findings" yang memuat: bentuk `OPERLOG` (`USER`/`USERPIC`/`BIOPHOTO`/`OPLOG`), `BIODATA` (Type 9/MajorVer 36 wajah, Type 8/MajorVer 20 palm), `options` (pemisah koma, `FaceVersion`, `PvVersion`, `FPVersion`, `*FunOn`, `VisilightFun`), kolom ATTLOG `Status` (255 tanpa tombol; 0–5 dengan tombol), kode verifikasi (15 wajah, 25 palm, 4 kartu, 3 password), serta `DATA DELETE`/`DATA UPDATE` yang bekerja. Tanpa data biometrik asli.
- [ ] **Step 5:** `make test && make lint` → hijau (cek `deadcode`: kedua `WithOn…` terpanggil tes).
- [ ] **Step 6: Commit** `"Add OPERLOG and BIODATA callbacks and parse options push"`.

---

## Tahap 1 — Kerangka modul

### Task 4: Kerangka modul `kapi/`

**Files:**
- Create: `kapi/go.mod`, `kapi/config.go`, `kapi/config_test.go`, `kapi/main.go` (sementara hanya memuat config dan keluar)
- Modify: `Makefile` (target `test` juga menjalankan `cd kapi && go test -race -cover ./...`), `.gitignore` (`/kapi/kapi`)

**Interfaces:**
- Produces: `type Config struct { DSN, Addr string; PollInterval, OnlineThreshold time.Duration; LogLevel slog.Level }` dan `func LoadConfig(getenv func(string) string, args []string) (Config, error)`.

- [ ] **Step 1: Tes gagal** (`package main`): `TestLoadConfig_RequiresDSN` (error bila `KAPI_DSN` kosong), `TestLoadConfig_Defaults` (`Addr==":8080"`, `PollInterval==2*time.Second`, `OnlineThreshold==2*time.Minute`, `LogLevel==slog.LevelInfo`), `TestLoadConfig_Overrides` (flag `-addr :9090 -poll 5s -online-threshold 90s -log-level debug`), `TestLoadConfig_InvalidLogLevel`.
- [ ] **Step 2:** `cd kapi && go test ./...` → FAIL.
- [ ] **Step 3: Implementasi** `LoadConfig` dengan `flag.NewFlagSet` (bukan global). `go.mod`: go 1.26.2 sama seperti `examples/database`, `replace` ke `../`. Jalankan `go get github.com/go-sql-driver/mysql` di `kapi/` hanya saat Task 5 membutuhkannya.
- [ ] **Step 4:** `make test` → PASS (root + `examples/database` + `kapi`).
- [ ] **Step 5: Commit** `"Scaffold kapi module with config loading"`.

### Task 5: Identitas — SN → alat, PIN → karyawan

**Files:**
- Create: `kapi/store/types.go`, `kapi/store/mysql.go` (`Open`, query identitas), `kapi/store/integration_test.go` (tag `integration`), `kapi/store/testdata/schema.sql`, `kapi/bridge/ports.go`, `kapi/bridge/resolve.go`, `kapi/bridge/resolve_test.go`, `kapi/internal/fakestore/fakestore.go`

**Interfaces:**
- Produces (`store`):
  - `type Device struct { ID, CompanyID, SerialNumber, Timezone, Firmware string; Active bool }`
  - `var ErrNotFound, ErrAmbiguous error`
  - `func Open(dsn string) (*MySQL, error)`; metode `ResolveDevice(ctx, sn string) (Device, error)`, `ListActiveDevices(ctx) ([]Device, error)`, `ResolveEmployee(ctx, companyID string, pin int64) (string, error)`, `EmployeeName(ctx, employeeID string) (string, error)`
- Produces (`bridge`): `type Identity interface` memuat keempat metode itu; `var ErrDeviceRejected, ErrEmployeeUnknown error`; `func NewResolver(id Identity, ttl time.Duration, now func() time.Time) *Resolver`; `(*Resolver).Device(ctx, sn string) (store.Device, error)` (menolak SN tak ada/ganda/nonaktif dengan `ErrDeviceRejected`); `(*Resolver).Employee(ctx, dev store.Device, pin string) (string, error)` (`ErrEmployeeUnknown` untuk PIN tak valid/tak ada/ambigu).
- `fakestore.New() *Store` mengimplementasikan seluruh ports yang ditambahkan tahap berikutnya; tiap task menambah metodenya.

- [ ] **Step 1: Skema uji.** Hasilkan `kapi/store/testdata/schema.sql` (hanya struktur) dari database sumber lewat klien read-only: `mysql --defaults-extra-file=$HOME/.config/kapi-ro.cnf --init-command="SET SESSION TRANSACTION READ ONLY" -B -N --raw -e "SHOW CREATE TABLE <t>"` untuk tabel: `mst_a_device`, `mst_h_employee`, `mst_h_employee_attendance`, `mst_a_device_employee`, `mst_a_device_cmd_log`, `mst_a_employee_att_temp_face`, `mst_a_employee_att_temp_palm`, `mst_a_employee_att_temp_fp`, `trx_a_presence`, `trx_a_process_attendance`. Pakai DDL apa adanya (MariaDB 10.3 sama dengan sumber); **tidak ada `INSERT`**. Pastikan kolom `type`, `major_ver`, `minor_ver` ada di tabel face dan palm (migrasi 001).
- [ ] **Step 2: Tes gagal** (`resolve_test.go`, pakai `fakestore`):
  - `TestResolver_TwoCompaniesSamePIN`: SN-A (company A) dan SN-B (company B) dengan PIN `"7"` menghasilkan `emp-A` dan `emp-B` yang berbeda.
  - `TestResolver_DeviceRejections`: SN tidak ada, `Active=false`, dan `ErrAmbiguous` semuanya → `errors.Is(err, ErrDeviceRejected)`.
  - `TestResolver_EmployeeAmbiguousOrUnknown` → `ErrEmployeeUnknown`.
  - `TestResolver_PinForms`: `"0042"` → dicari sebagai 42; tabel `"0"`, `"-1"`, `"abc"`, `""`, `"  "`, `"99999999999999999999"` → `ErrEmployeeUnknown` dan store **tidak** dipanggil.
  - `TestResolver_DeviceCacheTTL`: panggilan kedua dalam TTL tidak menyentuh store; setelah TTL menyentuh lagi.
- [ ] **Step 3: Tes integrasi** (tag `integration`): `TestMySQLIdentity_ResolveEmployeeScopesByCompany` (dua perusahaan, PIN sama, hasil berbeda; `is_delete=1` dan PIN NULL/0 tidak ketemu), `TestMySQLIdentity_ResolveDevice` (satu, nol, dua baris → hasil/`ErrNotFound`/`ErrAmbiguous`). Harness `openTestDB(t)` membaca `KAPI_TEST_DSN`, `t.Skip` bila kosong, **gagal bila nama database mengandung `prod`**, memuat `schema.sql` ke database kosong.
- [ ] **Step 4:** `cd kapi && go test ./...` → FAIL.
- [ ] **Step 5: Implementasi.** Kueri: `ResolveDevice` → `SELECT device_id, company_id, device_sn, IFNULL(timezone,''), IFNULL(is_active,0), IFNULL(firmware,'') FROM mst_a_device WHERE device_sn = ?` (0 baris `ErrNotFound`, >1 `ErrAmbiguous`); `ResolveEmployee` → `SELECT a.employee_id FROM mst_h_employee e JOIN mst_h_employee_attendance a ON a.employee_id = e.employee_id WHERE e.company_id = ? AND a.pin_id = ? AND IFNULL(e.is_delete,0) = 0 LIMIT 2` (`pin <= 0` langsung `ErrNotFound` tanpa kueri); `ListActiveDevices` memakai `is_active = 1`. `Open` memakai `sql.Open("mysql", dsn)`, `SetMaxOpenConns(10)`, `Ping`.
- [ ] **Step 6:** `make test` PASS; jalankan integrasi: `docker run -d --name kapi-test-db -e MARIADB_ROOT_PASSWORD=test -e MARIADB_DATABASE=kapi_unit -p 33306:3306 mariadb:10.3` lalu `KAPI_TEST_DSN='root:test@tcp(127.0.0.1:33306)/kapi_unit' go test -tags integration ./store/...` → PASS.
- [ ] **Step 7: Commit** `"Add store identity queries and company-scoped resolver"`.

### Task 6: Pengulangan untuk error sementara

**Files:**
- Create: `kapi/bridge/retry.go`, `kapi/bridge/retry_test.go`

**Interfaces:**
- Produces: `type RetryPolicy struct { Initial, Max time.Duration; Sleep func(ctx context.Context, d time.Duration) error }`, `func DefaultRetryPolicy() RetryPolicy` (1 s, 30 s), `func IsTransient(err error) bool`, `func (p RetryPolicy) Do(ctx context.Context, op func() error) error`.

- [ ] **Step 1: Tes gagal:** `TestRetry_TransientThenSuccess` (op gagal 2× lalu sukses → dipanggil 3×, `Sleep` direkam `[1s, 2s]`), `TestRetry_BackoffCapsAtMax` (6 kegagalan → `[1s,2s,4s,8s,16s,30s,30s]` untuk 7 jeda), `TestRetry_PermanentReturnsImmediately`, `TestRetry_ContextCancelDuringSleep` (kembalikan `ctx.Err()`), `TestIsTransient` (tabel: `driver.ErrBadConn`, `io.EOF`, `*net.OpError`, `&mysql.MySQLError{Number:1205}`, `1213`, `2006`, `2013` → true; `&mysql.MySQLError{Number:1062}`, `errors.New("x")`, `context.Canceled` → false).
- [ ] **Step 2:** `go test ./bridge -run 'Retry|IsTransient'` → FAIL.
- [ ] **Step 3: Implementasi** sesuai signature; `Sleep` bawaan memakai `time.NewTimer` dan `ctx.Done()`. Nomor error sementara: 1040, 1053, 1205, 1213, 2002, 2003, 2006, 2013.
- [ ] **Step 4:** `go test -race ./...` PASS. **Step 5: Commit** `"Add transient-error retry helper"`.

### Task 7: Kemampuan alat, sinkronisasi alat, dan status 0/1/2

**Files:**
- Create: `kapi/bridge/capabilities.go`, `kapi/bridge/devsync.go`, `kapi/bridge/status.go` dan tes masing-masing
- Modify: `kapi/bridge/ports.go`, `kapi/store/mysql.go`, `kapi/internal/fakestore/fakestore.go`

**Interfaces:**
- Produces:
  - `type Capabilities struct { Firmware, IP, DeviceName string; FPVersion, FaceVersion, FvVersion, PvVersion int; FingerOn, FaceOn, FvOn, PvOn, Visilight bool }`
  - `func ParseCapabilities(info map[string]string) Capabilities` (kunci: `FWVersion`, `IPAddress`, `DeviceName`, `FPVersion`, `FaceVersion`, `FvVersion`, `PvVersion`, `FingerFunOn`, `FaceFunOn`, `FvFunOn`, `PvFunOn`, `VisilightFun`)
  - `func (c Capabilities) Accepts(typ, major int) bool` — 1: `FingerOn && FPVersion==major`; 2: `FaceOn && !Visilight && FaceVersion==major`; 9: `FaceOn && Visilight && FaceVersion==major`; 8: `PvOn && PvVersion==major`; 7: `FvOn && FvVersion==major`; lainnya false
  - `type CapabilityCache` dengan `Set(sn string, c Capabilities)` dan `Get(sn string) (Capabilities, bool)`
  - `func ParseTimezone(s string) (*time.Location, error)` (`"+07:00"` → `time.FixedZone("+07:00", 25200)`)
  - `type Server interface { ListDevices() []*zkadms.Device; IsDeviceOnline(sn string) bool; PendingCommandsCount(sn string) int; QueueCommand(sn, cmd string) (int64, error); RegisterDevice(sn string, opts ...zkadms.DeviceOption) error; SetDeviceTimezone(sn string, loc *time.Location) error }`
  - `type StatusStore interface { SetDeviceStatus(ctx, sn string, st store.DeviceStatus) error; CountPendingCommands(ctx, sn string) (int, error) }`, `store.DeviceStatus{Code int; Description, IP, Firmware, DeviceNameInfo string}`
  - `func NewTracker(srv Server, st StatusStore, caps *CapabilityCache, now func() time.Time, log *slog.Logger) *Tracker`; `(*Tracker).Tick(ctx)`, `(*Tracker).OnDeviceInfo(ctx, sn string, info map[string]string)` (menyimpan `ParseCapabilities(info)` ke `CapabilityCache` lalu memaksa penulisan status), `(*Tracker).Close(ctx)`; `func SyncDevices(ctx context.Context, id Identity, srv Server, log *slog.Logger) error`

- [ ] **Step 1: Tes gagal:**
  - `TestParseCapabilities_ZAM230`: map hasil capture (`FWVersion=ZAM230-NF50VA-Ver1.3.7`, `IPAddress=192.168.1.99`, `DeviceName=FA01`, `FPVersion=10`, `FaceVersion=36`, `FvVersion=3`, `PvVersion=20`, `FingerFunOn=0`, `FaceFunOn=1`, `FvFunOn=0`, `PvFunOn=1`, `VisilightFun=1`) → field sesuai.
  - `TestCapabilities_Accepts`: ZAM230: `(9,36)` true, `(8,20)` true, `(2,12)` false, `(1,10)` false (`FingerFunOn=0`), `(9,35)` false, `(7,3)` false.
  - `TestParseTimezone`: `"+07:00"`→25200 detik, `"-03:30"`→-12600, `""`/`"garbage"`/`"+7"` → error.
  - `TestSyncDevices_RegistersWithTimezone`: dua alat aktif (`+07:00`, dan `garbage` → UTC + peringatan) terdaftar di `Server` dengan zona yang benar; alat nonaktif tidak.
  - `TestTracker_WritesOnlyOnTransition`: online tanpa antrian selama 5 tick → tepat satu `SetDeviceStatus` (kode 1, `Connect`); alat offline → tepat satu penulisan kode 0 (`Not Connect`); online dengan antrian 3 → kode 2, `Process Sync (3 menunggu)`; perubahan angka <5 dtk tidak menulis, ≥5 dtk menulis; antrian kosong → kembali kode 1.
  - `TestTracker_OnlyTouchesDevicesTheServerKnows`: SN yang tidak ada di `ListDevices` tak pernah ditulis.
  - `TestTracker_CloseMarksTrackedDevicesOffline`.
  - `TestTracker_OnDeviceInfoWritesIPFirmwareName`: `OnDeviceInfo` dengan `ParseCapabilities` memicu penulisan yang memuat IP, firmware, dan nama.
- [ ] **Step 2:** `go test ./bridge` → FAIL.
- [ ] **Step 3: Implementasi.** `Tracker` menyimpan keadaan terakhir per SN; antrian = `st.CountPendingCommands` + `srv.PendingCommandsCount`. Kueri MySQL: `SetDeviceStatus` → `UPDATE mst_a_device SET status=?, description=?, updated_date=NOW(), updated_by='Logger' [, device_ip=?, firmware=?, device_name_info=? bila tidak kosong] WHERE device_sn=?`; `CountPendingCommands` → `SELECT COUNT(*) FROM mst_a_device_cmd_log WHERE serial_number=? AND transaction_time IS NULL AND return_value IS NULL`. `SyncDevices` memakai `RegisterDevice(sn, zkadms.WithDeviceTimezone(loc))`, atau `SetDeviceTimezone` bila sudah terdaftar.
- [ ] **Step 4:** `go test -race ./...` PASS; tambahkan tes integrasi `TestMySQLSetDeviceStatus` dan `TestMySQLCountPendingCommands`. **Step 5: Commit** `"Track device capabilities and online/offline/sync status"`.

---

## Tahap 2 — Absensi

### Task 8: Absensi → `trx_a_presence`

**Files:**
- Create: `kapi/bridge/bridge.go` (struct `Bridge` dengan field minimal `resolver *Resolver`, `presence PresenceStore`, `retry RetryPolicy`, `log *slog.Logger`; Task 9–14 menambah field), `kapi/bridge/attendance.go`, `kapi/bridge/attendance_test.go`
- Modify: `kapi/bridge/ports.go`, `kapi/store/types.go` (`Presence`), `kapi/store/mysql.go`, `kapi/internal/fakestore/fakestore.go`, `kapi/store/integration_test.go`

**Interfaces:**
- Consumes: `Resolver`, `RetryPolicy` (Task 5–6).
- Produces: `store.Presence{CompanyID, EmployeeID, DeviceSN string; PIN int64; At time.Time; Flag int}`; `type PresenceStore interface { AddPresence(ctx, p store.Presence) (inserted bool, err error) }`; `func FlagFor(status, verify int) int`; `(*Bridge).onAttendance(ctx context.Context, rec zkadms.AttendanceRecord)` (struktur `Bridge` dibuat di `bridge.go` pada task ini).

- [ ] **Step 1: Tes gagal:**
  - `TestFlagFor`: `(0,25)→0`, `(1,25)→1`, `(5,15)→5`, `(255,15)→15`, `(6,25)→25`, `(-1,3)→3`.
  - `TestAttendance_TruncatesToMinute`: `13:20:05` menjadi `13:20:00` pada `Presence.At`.
  - `TestAttendance_TwoCompaniesSamePIN`: SN-A dan SN-B, PIN `"7"` → dua `Presence` dengan `CompanyID`/`EmployeeID` masing-masing.
  - `TestAttendance_UnknownDeviceOrPinNeverWrites`: SN tak dikenal, nonaktif, PIN `"abc"`, PIN tanpa karyawan → `AddPresence` tidak dipanggil, tidak panik.
  - `TestAttendance_DuplicateMinuteReportedNotInserted`: dua punch semenit → panggilan kedua mengembalikan `inserted=false` dan tidak error.
  - `TestAttendance_TransientErrorRetriedNoLossNoDuplicate`: rombongan 3 absen; store gagal sementara 2× pada absen kedua → akhirnya tepat 3 `Presence` tersimpan, urutan terjaga (`Sleep` ditiru).
  - Integrasi `TestMySQLAddPresence`: menulis satu baris `trx_a_presence` (`type_device='Device'`, `created_by='Logger'`, `approved_status_id` konstanta, `device_name`=SN) **dan** satu `trx_a_process_attendance` (`first_date` 00:00:00, `end_date` 23:59:59, `period` `Oct 2026`, `created_by='Auto Process'`, `updated_by='Push Logger'`); panggilan kedua pada menit sama `inserted=false` dan tidak menambah baris.
- [ ] **Step 2:** `go test ./bridge -run 'Attendance|FlagFor'` → FAIL.
- [ ] **Step 3: Implementasi.** `AddPresence` dalam satu transaksi: `SELECT 1 FROM trx_a_presence WHERE employee_id=? AND presence_date=? LIMIT 1`; bila ada → `false`; selain itu `INSERT` presence lalu process. Waktu dikirim sebagai string `At.Format("2006-01-02 15:04:00")` (tanpa konversi zona oleh driver); GUID dari `crypto/rand` (fungsi `store.NewID() string`, format UUID v4, huruf kecil). Panggil store lewat `RetryPolicy.Do`.
- [ ] **Step 4:** `make test` PASS (integrasi dengan tag). **Step 5: Commit** `"Write attendance to trx_a_presence with minute dedupe"`.

---

## Tahap 3 — User dan template

### Task 9: Kredensial user (`OPERLOG` `USER`)

**Files:**
- Create: `kapi/bridge/credentials.go`, `kapi/bridge/credentials_test.go`
- Modify: `kapi/bridge/ports.go`, `kapi/store/types.go`, `kapi/store/mysql.go`, `kapi/internal/fakestore/fakestore.go`

**Interfaces:**
- Produces: `store.Credentials{CardNo, Password, Priv string}`; `type CredentialStore interface { UpdateEmployeeCredentials(ctx, employeeID string, c store.Credentials) error }`; `func NormalizeCard(s string) string` (`"[0000000000]"` → `"0"`, lainnya apa adanya); `(*Bridge).onOperLog(ctx context.Context, sn string, batch zkadms.OperLogBatch)` (menangani `Users`; `Photos` ditangani Task 10); titik sambung nil-aman `Bridge.userHook func(ctx context.Context, employeeID, srcSN string, pin int64, c store.Credentials)` yang diisi Task 11.

- [ ] **Step 1: Tes gagal:** `TestNormalizeCard`; `TestUsers_UpdatesCredentialsForKnownEmployee` (`Card:"[0000000000]"`→`CardNo=="0"`, `Privilege:14`→`Priv=="14"`, `Password` diteruskan); `TestUsers_UnknownPINIgnored` (tidak ada pembaruan, tidak ada hook); `TestUsers_HookCalledOncePerKnownUser`. Integrasi `TestMySQLUpdateEmployeeCredentials` (kolom `card_no`, `password_device`, `priv`, `updated_by='Logger'` terisi; baris lain tak berubah).
- [ ] **Step 2:** `go test ./bridge -run 'Users|NormalizeCard'` → FAIL. **Step 3: Implementasi** dengan `UPDATE mst_h_employee_attendance SET card_no=?, password_device=?, priv=?, updated_date=NOW(), updated_by='Logger' WHERE employee_id=?`. **Step 4:** PASS. **Step 5: Commit** `"Update employee credentials from device users"`.

### Task 10: Template dan foto → tabel final

**Files:**
- Create: `kapi/bridge/templates.go`, `kapi/bridge/templates_test.go`
- Modify: `kapi/bridge/ports.go`, `kapi/store/types.go`, `kapi/store/mysql.go`, `kapi/internal/fakestore/fakestore.go`, `kapi/bridge/credentials.go` (rute `Photos` di `onOperLog`)

**Interfaces:**
- Produces:
  - `store.Face{Index int; IsBiodata bool; Type, MajorVer, MinorVer *int; Template string; Size int}`, `store.Palm{Index int; Type, MajorVer, MinorVer *int; Template string}`, `store.Finger{Version string; Slot int; Template string}`
  - `type TemplateStore interface { UpsertFace(ctx, employeeID string, f store.Face) (changed bool, err error); UpsertPalm(ctx, employeeID string, p store.Palm) (changed bool, err error); UpsertFinger(ctx, employeeID string, f store.Finger) (changed bool, err error); RecountTemplates(ctx, employeeID string) error }`
  - `(*Bridge).onBioData(ctx context.Context, sn string, ts []zkadms.BioTemplate)`; titik sambung `Bridge.templateHook func(ctx context.Context, employeeID, srcSN string, pin int64, items []TemplateItem)` dengan `type TemplateItem struct { Face *store.Face; Palm *store.Palm; Finger *store.Finger }` (hanya item yang `changed`).

- [ ] **Step 1: Tes gagal (logika):** `TestTemplates_Type9Face` (→ `UpsertFace{Index:0, IsBiodata:true, Type:9, MajorVer:36, MinorVer:1, Size:0}`); `TestTemplates_Type2Face`; `TestTemplates_Type8Palm` (`MajorVer 20`); `TestTemplates_Type1Finger` (`No=2`→`Slot==3`, `Version=="10"`; `MajorVer` 0 memakai `FPVersion` dari `CapabilityCache`, kosong → `"10"`); `TestTemplates_OtherTypesIgnored` (Type 6, 7 tidak memanggil store); `TestTemplates_PhotosShareIndex100` (`USERPIC` dan `BIOPHOTO` → `UpsertFace{Index:100, IsBiodata:true, Type:nil, Size:len(Content)}` keduanya); `TestTemplates_UnknownPINIgnored`; `TestTemplates_OnlyChangedItemsReachHook`; `TestTemplates_RecountedAfterChange`.
- [ ] **Step 2: Tes integrasi:** `TestMySQLUpsertFace_LegacyRowNotOverwrittenByNewType` (ada baris legacy `index 0`, `type NULL`; masuk Type 9 `index 0` → dua baris, legacy utuh; masuk Type 2 `index 0` → memperbarui baris legacy dan mengisi `type=2`); `TestMySQLUpsertFace_UnchangedReportsFalse`; `TestMySQLUpsertPalm_*` serupa (efektif `COALESCE(type,8)`); `TestMySQLUpsertFinger_FillsSlotColumn` (`Slot 3` → kolom `fp3`, baris dikunci `(employee_id, vx)`); `TestMySQLRecountTemplates_Semantics` (`total_face` = jumlah semua baris face karyawan termasuk foto, `total_palm` = jumlah baris palm, `total_fp9/10/12` = slot `fp1..fp10` tak kosong per `vx`).
- [ ] **Step 3:** `go test` → FAIL. **Step 4: Implementasi.** Kunci cari face: template (`Index < 100`) → `employee_id=? AND index=? AND is_biodata=? AND COALESCE(type,2)=?`; foto (`Index == 100`) → `... AND type IS NULL`. Palm: `COALESCE(type,8)=?`. `changed` = tidak ada baris sebelumnya atau konten/jenis/versi berbeda; saat update isi `updated_date/updated_by='Logger'` dan `type/major_ver/minor_ver`. Insert: `created_by='Logger'`, GUID baru. Semua `index` memakai backtick di SQL.
- [ ] **Step 5:** `make test` PASS. **Step 6: Commit** `"Store device templates and photos with type and version"`.

---

## Tahap 4 — Penyebaran

### Task 11: Penyebaran user dan template ke alat lain

**Files:**
- Create: `kapi/bridge/commands.go`, `kapi/bridge/commands_test.go`, `kapi/bridge/fanout.go`, `kapi/bridge/fanout_test.go`
- Modify: `kapi/bridge/ports.go`, `kapi/store/types.go`, `kapi/store/mysql.go`, `kapi/internal/fakestore/fakestore.go`, `kapi/bridge/bridge.go` (isi `userHook` dan `templateHook`)

**Interfaces:**
- Produces:
  - Pembangun command (murni): `UserInfoCommand(pin int64, name, priv, password, card string) string`; `BioDataCommand(pin int64, no int, index string, typ, major, minor int, tmp string) string`; `FingerBioDataCommand(pin int64, no int, major int, tmp string) string` (format lama: `Index=` dan `MinorVer=`/`Format=` kosong); `FingerTmpCommand(pin int64, fid int, tmp string) string`; `UserPicCommand(pin int64, content string) string`; `BioPhotoCommand(pin int64, content string) string`; `sanitizeField(s string) string` (tab/CR/LF → spasi).
  - `store.NewCommand{SerialNumber, PIN, Action, Content string; DedupeContent bool}`; `type FanoutStore interface { Targets(ctx, employeeID, excludeSN string) ([]string, error); EnqueueCommand(ctx, c store.NewCommand) (inserted bool, err error) }` (memakai `Identity.EmployeeName`)
  - `type Fanout struct{…}`; `func NewFanout(fs FanoutStore, id Identity, caps *CapabilityCache, retry RetryPolicy, log *slog.Logger) *Fanout`; `(*Fanout).SendUser(ctx, employeeID, srcSN string, pin int64, c store.Credentials) error`; `(*Fanout).SendTemplates(ctx, employeeID, srcSN string, pin int64, items []TemplateItem) error`

- [ ] **Step 1: Tes gagal (format persis):**
  - `TestUserInfoCommand`: `UserInfoCommand(9001,"Ann","0","123456","2252991") == "DATA UPDATE USERINFO PIN=9001\tName=Ann\tPri=0\tPasswd=123456\tCard=2252991\tGrp=0\tTZ=0"`.
  - `TestBioDataCommand_Face36`: `== "DATA UPDATE BIODATA Pin=9001\tNo=0\tIndex=0\tValid=1\tDuress=0\tType=9\tMajorVer=36\tMinorVer=1\tFormat=0\tTmp=QUJD"`.
  - `TestPhotoCommands`: `"DATA UPDATE USERPIC PIN=9001\tSize=4\tContent=QUJD"` dan `"DATA UPDATE BIOPHOTO PIN=9001\tType=9\tSize=4\tContent=QUJD\tFormat=0\tUrl=\tPostBackTmpFlag=0"` (Size = panjang string konten; contoh memakai konten 4 karakter `QUJD`).
  - `TestFingerCommands`: `FingerTmpCommand(9001,2,"AB") == "DATA UPDATE FINGERTMP PIN=9001\tFID=2\tSize=2\tValid=1\tTMP=AB"`; `FingerBioDataCommand(9001,2,12,"AB") == "DATA UPDATE BIODATA Pin=9001\tNo=2\tIndex=\tValid=1\tDuress=0\tType=1\tMajorVer=12\tMinorVer=\tFormat=\tTmp=AB"`.
  - `TestSanitizeField_RemovesControlChars`.
  - Perilaku: `TestFanout_SendUserToOtherDevicesOnly` (target `[B,C]`; `Action=="Send User"`, `DedupeContent` true, `Name` dari `EmployeeName`); `TestFanout_SendUserDedupeSkipIsNotError`; `TestFanout_LegacyTemplateSentRegardlessOfCapabilities` (`Type==nil` → `Type=2/MajorVer=12/MinorVer=0` untuk wajah, `Type=8/12/0` untuk palm, foto → dua command `USERPIC` + `BIOPHOTO`, `Action` `Send Face 100`); `TestFanout_NewTemplateOnlyWhenCapabilityAccepts` (B memakai kapabilitas ZAM230 → terkirim; C `FaceVersion=12` → dilewati); `TestFanout_UnknownCapabilitiesSkipped` (alat tanpa `options` → dilewati, tidak error); `TestFanout_NoTargetsIsNoop`. Aksi: `Send User`, `Send Face <index>`, `Send Palm <index>`, `Send Finger <slot>`.
  - Integrasi: `TestMySQLTargets_SameCompanyOnly` (alat lain milik karyawan itu di perusahaan sama; alat sumber dan alat perusahaan lain tidak), `TestMySQLEnqueueCommand_InsertsRowWithConstants` (`id` GUID, `trx_id=0`, `type_log`/`created_by` `Auto Update`, `commite_time` terisi), `TestMySQLEnqueueCommand_DedupeBySerialTypeContent`.
- [ ] **Step 2:** `go test ./bridge -run 'Command|Fanout|Sanitize'` → FAIL. **Step 3: Implementasi.** `Targets` → `SELECT d.device_sn FROM mst_a_device_employee de JOIN mst_a_device d ON d.device_id=de.device_id JOIN mst_h_employee e ON e.employee_id=de.employee_id WHERE de.employee_id=? AND d.company_id=e.company_id AND d.device_sn IS NOT NULL AND d.device_sn <> ?`. `EnqueueCommand` dengan dedupe: `INSERT … SELECT … FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM mst_a_device_cmd_log WHERE serial_number=? AND type_log='Auto Update' AND content=?)`. Field teks (nama) lewat `sanitizeField`. **Step 4:** PASS. **Step 5: Commit** `"Fan out users and compatible templates to other devices"`.

---

## Tahap 5 — Command

### Task 12: Poller antrian command

**Files:**
- Create: `kapi/bridge/poller.go`, `kapi/bridge/poller_test.go`
- Modify: `kapi/bridge/ports.go`, `kapi/store/types.go`, `kapi/store/mysql.go`, `kapi/internal/fakestore/fakestore.go`

**Interfaces:**
- Produces: `store.PendingCommand{ID, SerialNumber, PIN, Action, Content string}`; `type CommandStore interface { ClaimCommands(ctx, sns []string, limit int) ([]store.PendingCommand, error); ReleaseCommand(ctx, id string) error; CompleteCommand(ctx, id string, o store.CommandOutcome) (store.CommandRow, error); RequeueOrphans(ctx, olderThan time.Duration) (int, error) }`; `store.CommandOutcome{ReturnValue, Message string; At time.Time}`; `store.CommandRow{ID, SerialNumber, PIN, Action string}`; `func NewPoller(srv Server, cs CommandStore, now func() time.Time, log *slog.Logger) *Poller`; `(*Poller).Tick(ctx)`; `(*Poller).Inflight() int`; berkas ini juga memiliki peta `inflight` (ID command library → `{CmdID, SN string; Sent time.Time}`) yang dipakai Task 13.

- [ ] **Step 1: Tes gagal:** `TestPoller_ClaimsOnlyForOnlineDevices` (server online `{A}`; `ClaimCommands` dipanggil dengan `[A]` dan `limit 100`); `TestPoller_QueuesAndTracksInflight` (`QueueCommand` menerima isi baris; `Inflight()==1`); `TestPoller_QueueFullReleasesRow` (`zkadms.ErrCommandQueueFull` → `ReleaseCommand`, `Inflight()==0`); `TestPoller_InvalidContentCompletedAsInvalidNoRetry` (`zkadms.ErrInvalidCommandField` → `CompleteCommand` dengan `ReturnValue=="Invalid"`, bukan `ReleaseCommand`); `TestPoller_SecondTickDoesNotResend` (klaim kedua kosong → `QueueCommand` sekali); `TestPoller_DeviceNotFoundReleasesRow`. Integrasi `TestMySQLClaimCommands_AtomicUnderConcurrency` (dua goroutine mengklaim set yang sama; tiap baris diklaim tepat sekali) dan `TestMySQLClaimCommands_OrderAndFilters` (urut `commite_time, id`; baris `transaction_time` terisi atau `return_value` terisi tidak diambil; hanya SN yang diminta).
- [ ] **Step 2:** FAIL. **Step 3: Implementasi.** `ClaimCommands`: `SELECT id, serial_number, IFNULL(pin_id,''), IFNULL(action,''), content FROM mst_a_device_cmd_log WHERE serial_number IN (…) AND transaction_time IS NULL AND return_value IS NULL ORDER BY commite_time, id LIMIT ?`, lalu per baris `UPDATE mst_a_device_cmd_log SET transaction_time=NOW() WHERE id=? AND transaction_time IS NULL`; hanya yang `RowsAffected()==1` dikembalikan. `ReleaseCommand` → `transaction_time=NULL`. Poller memakai `Server.ListDevices` + `IsDeviceOnline` untuk daftar SN online.
- [ ] **Step 4:** PASS. **Step 5: Commit** `"Poll the command queue and claim rows atomically"`.

### Task 13: Hasil command, batas waktu, dan alokasi

**Files:**
- Create: `kapi/bridge/results.go`, `kapi/bridge/results_test.go`
- Modify: `kapi/bridge/poller.go` (`Sweep`), `kapi/bridge/ports.go`, `kapi/store/mysql.go`, `kapi/internal/fakestore/fakestore.go`

**Interfaces:**
- Consumes: peta `inflight` dan `CompleteCommand` (Task 12).
- Produces: `type AllocationStore interface { MarkAllocationSent(ctx, cmdID string) error; ClearAllocations(ctx, sn string) error }`; `(*Bridge).onCommandResult(ctx context.Context, r zkadms.CommandResult)`; `(*Poller).Sweep(ctx)`.

- [ ] **Step 1: Tes gagal:** `TestResults_SuccessRecordedAndAllocationMarked` (`ReturnCode 0` → `ReturnValue "0"`, pesan `Pin id : 9001 Success`, `MarkAllocationSent(cmdID)`); `TestResults_NegativeCodeFailureKeepsAllocation` (`-1002` → `ReturnValue "-1002"`, pesan `Pin id : 9001 Failed!!`, `MarkAllocationSent` dan `ClearAllocations` **tidak** dipanggil); `TestResults_DeviceClearUserClearsAllocations` (aksi `Device Clear User`, sukses → `ClearAllocations(sn)`); `TestResults_UnknownWireIDIgnored`; `TestResults_NoPinMessages` (`Success` / `Failed Content not register!!`); `TestSweep_RequeuesThenTimesOut` (>10 menit tanpa balasan → `ReleaseCommand` hingga 3 kali; percobaan ke-4 → `CompleteCommand` `ReturnValue=="Timeout"`); `TestSweep_FreshInflightUntouched`. Integrasi `TestMySQLCompleteCommand_WritesColumns` (`return_value`, `respone_time`, `message`), `TestMySQLMarkAllocationSent` (hanya baris dengan `device_employee_id` = id command), `TestMySQLClearAllocations_ScopedToDevice`, `TestMySQLRequeueOrphans` (baris `transaction_time` lebih tua dari batas dan `return_value` NULL kembali NULL).
- [ ] **Step 2:** FAIL. **Step 3: Implementasi.** Sukses bila `ReturnCode >= 0`. Percobaan ulang dihitung di memori per `CmdID` (hilang saat restart; `RequeueOrphans` 10 menit dijalankan sekali saat startup). **Step 4:** PASS. **Step 5: Commit** `"Record command results with timeout and allocation updates"`.

---

## Tahap 6 — Perangkaian dan pengerasan

### Task 14: Perangkaian `Bridge`, `main`, CI, dan dokumentasi operasional

**Files:**
- Create: `kapi/bridge/bridge_test.go`, `kapi/README.md`
- Modify: `kapi/bridge/bridge.go` (lengkapi `Deps`, `New`, `Options`, `Attach`, `Run`, `Close`)
- Modify: `kapi/main.go`, `Makefile` (`lint-golangci-lint` juga menjalankan linter di `kapi/`; pastikan `make check-ci` tetap lulus), `.github/workflows/test.yml` (jalankan tes `kapi/`)

**Interfaces:**
- Produces: `type Deps struct { Identity Identity; Presence PresenceStore; Creds CredentialStore; Templates TemplateStore; Fanout FanoutStore; Commands CommandStore; Alloc AllocationStore; Status StatusStore; Log *slog.Logger; Now func() time.Time; Retry RetryPolicy }`; `func New(d Deps) *Bridge`; `(*Bridge).Options() []zkadms.Option` (mendaftarkan `WithOnAttendance`, `WithOnOperLog`, `WithOnBioData`, `WithOnDeviceInfo`, `WithOnCommandResult`); `(*Bridge).Attach(srv Server)`; `(*Bridge).Run(ctx context.Context, poll time.Duration)` (loop `Tick` poller+tracker tiap `poll`, `SyncDevices` tiap 60 dtk, `Sweep` tiap tick); `(*Bridge).Close(ctx context.Context)`.

- [ ] **Step 1: Tes gagal (end-to-end dengan `fakestore` dan `zkadms.ADMSServer` asli lewat `httptest`):** `TestBridge_AttendanceTwoCompaniesSamePIN` (POST ATTLOG untuk SN-A dan SN-B, PIN sama → `Presence` ke perusahaan masing-masing); `TestBridge_OptionsThenOperLogAndBioData` (POST `options` → kapabilitas tersimpan dan status ditulis; POST `OPERLOG` (`USER`+`USERPIC`) dan `BIODATA` → kredensial, `UpsertFace` type 9, `UpsertFace` index 100); `TestBridge_CommandRoundTrip` (baris `PendingCommand` untuk alat online → GET `/iclock/getrequest?SN=…` mengembalikan `C:<id>:<content>`; POST `/iclock/devicecmd` dengan `ID=<id>&Return=0&CMD=DATA` → `CompleteCommand` dipanggil); `TestBridge_UnknownSerialNeverWrites` (semua jenis kiriman dari SN tak dikenal tidak menyebabkan penulisan); `TestBridge_CloseMarksDevicesOffline`.
- [ ] **Step 2:** FAIL. **Step 3: Implementasi** perangkaian. `main.go`: `LoadConfig` → `store.Open` → `bridge.New` → `zkadms.NewADMSServer(append(b.Options(), zkadms.WithMaxBodySize(16<<20), zkadms.WithOnlineThreshold(cfg.OnlineThreshold), zkadms.WithLogger(log))...)` → `b.Attach(srv)` → `RequeueOrphans(ctx, 10*time.Minute)` → `go b.Run(ctx, cfg.PollInterval)` → `http.Server{Addr, Handler: srv}`; sinyal `SIGINT`/`SIGTERM` → `Shutdown` (10 dtk), `b.Close`, `srv.Close`.
- [ ] **Step 4: Dokumentasi `kapi/README.md`:** variabel env dan flag; **hak akses database minimum** (`SELECT` pada `mst_a_device`, `mst_h_employee`, `mst_h_employee_attendance`, `mst_a_device_employee`; `INSERT` pada `trx_a_presence`, `trx_a_process_attendance`; `INSERT, UPDATE` pada `mst_a_employee_att_temp_face`, `_palm`, `_fp`, `mst_a_device_cmd_log`; `UPDATE` pada `mst_a_device`, `mst_h_employee_attendance`; `UPDATE, DELETE` pada `mst_a_device_employee`); cara menjalankan tes integrasi; peralihan per alat dan rollback; **pemulihan data PIN yang belum terdaftar** (`DATA QUERY USERINFO [PIN=n]`, `DATA QUERY ATTLOG` dengan rentang waktu); batasan yang diketahui (jari belum teruji di perangkat keras; baris `Device Employee` buatan web KAPI masih memakai `Type=2/MajorVer=12`).
- [ ] **Step 5: CI:** tambah langkah uji untuk `kapi/` di workflow dan target lint; jalankan `make test && make lint && make check-ci` → hijau.
- [ ] **Step 6: Penerimaan di alat sungguhan** (manual, database uji **lokal**, bukan `kapi_test_prod2`): jalankan MariaDB 10.3 lokal berskema Task 5 dengan satu perusahaan, satu alat (`device_sn=8764261200542`, `is_active=1`, `timezone='+07:00'`), satu karyawan ber-PIN 9001 tertaut `mst_h_employee_attendance`; arahkan alat ke server ini. Periksa berurutan: (a) `status` alat 1 dan `firmware`/`device_ip` terisi; (b) absen palm → satu baris `trx_a_presence` `flag=25` dan satu `trx_a_process_attendance`; (c) tombol In lalu absen → `flag=0`; (d) daftarkan wajah dan palm di alat → baris face (`type=9`, `major_ver=36`) dan palm (`type=8`, `major_ver=20`) terisi, `total_face`/`total_palm` benar; (e) sisipkan baris `mst_a_device_cmd_log` `DATA QUERY USERINFO PIN=9001` untuk SN itu → ia terkirim, dibalas, dan `return_value='0'`; (f) matikan MariaDB sebentar saat absen → absen muncul setelah pulih tanpa duplikat.
- [ ] **Step 7: Commit** `"Wire the KAPI bridge, add docs and CI for the kapi module"`.

---

## Catatan eksekusi
- Tahap 0 menyentuh library publik dan harus selesai (dan dikomit) sebelum Tahap 1.
- Urutan tugas mengikuti ketergantungan antarmuka: Task 8 membuat struct `Bridge` minimal; Task 9–11 hanya menambah field/hook; Task 14 menyatukan.
- Penulisan ke database bersama (termasuk `kapi_test_prod2`) tidak termasuk rencana ini; peralihan produksi dikerjakan terpisah setelah penerimaan Task 14 dan atas izin tegas pemilik proyek.
