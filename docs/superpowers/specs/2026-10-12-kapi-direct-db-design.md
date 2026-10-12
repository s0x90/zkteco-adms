# Desain: Integrasi langsung ADMS → database KAPI

Tanggal: 2026-10-12
Status: menunggu tinjauan pemilik proyek

## 1. Tujuan dan ruang lingkup

Menggantikan dua aplikasi yang sekarang harus dipelihara (logger lama "Push Logger" C#/Python dan hop REST `KAPI_API_PUSHLOGGER`) dengan satu server ADMS berbasis library ini, yang **menulis langsung ke database KAPI (MariaDB)**.

**Termasuk:**
- Absensi (ATTLOG) → `trx_a_presence` dan `trx_a_process_attendance`.
- User, password, kartu, template wajah / palm / jari, dan foto dari alat → tabel final KAPI.
- Penyebaran user dan template ke alat lain milik karyawan yang sama.
- Antrian command KAPI → alat (`mst_a_device_cmd_log`) dan pencatatan hasilnya.
- Status alat online / offline / "Process Sync" di `mst_a_device`.
- Pemisahan data per perusahaan (alat memiliki `company_id` berbeda dan PIN bisa sama).

**Tidak termasuk:** perhitungan absensi, pembuatan jadwal, dan notifikasi Firebase (tetap di API KAPI, yang membaca `trx_a_process_attendance`); SMS, WorkCode, ATTPHOTO, dan USB log dari logger lama.

## 2. Fakta terverifikasi yang menjadi dasar

### 2.1 Alat (ZAM230-NF50VA, firmware Ver1.3.7, diuji pada alat nyata)
- Setiap request membawa `SN` di query string.
- `table=ATTLOG`: `PIN \t waktu \t Status \t Verify \t WorkCode ...`. `Status` = 0 In, 1 Out, 2 Break Out, 3 Break In, 4 OT In, 5 OT Out **hanya bila tombol status ditekan**; selain itu `255`, dan alat kembali ke 255 setelah satu absen. Kode verifikasi: 15 wajah, 25 palm, 4 kartu, 3 password.
- `table=OPERLOG`: baris `USER PIN=..\tName=..\tPri=..\tPasswd=..\tCard=..\tGrp=..\tTZ=..`, `USERPIC PIN=..\tFileName=..\tSize=..\tContent=<base64 jpeg>`, `BIOPHOTO PIN=..\tFileName=..\tType=9\tSize=..\tContent=<base64 jpeg>`, dan baris `OPLOG <kode>` (log operasi).
- `table=BIODATA`: `BIODATA Pin=..\tNo=..\tIndex=..\tValid=..\tDuress=..\tType=..\tMajorVer=..\tMinorVer=..\tFormat=..\tTmp=<base64>`. Wajah cahaya tampak: `Type=9, MajorVer=36, MinorVer=1` (±404 karakter). Palm: `Type=8, MajorVer=20, MinorVer=1` (±416 karakter). Alat ini tidak punya sensor jari.
- Alat mendorong `USER`, wajah, dan palm sendiri saat pendaftaran. Foto hanya datang lewat `DATA QUERY USERINFO`, yang juga mengirim semua user dan template. Data yang sama bisa datang dua kali.
- `DATA DELETE BIODATA|USERPIC|BIOPHOTO|USERINFO PIN=n` dan `DATA UPDATE USERINFO|BIODATA|USERPIC|BIOPHOTO` bekerja (`Return=0`), termasuk command foto ±58 KB. Setelah dikirim balik, palm dan foto identik byte per byte; template wajah berubah isinya (deterministik) tetapi tetap bisa dipakai verifikasi.
- `Return=0` tidak berarti ada data; filter `Type` pada `DATA QUERY BIODATA` bekerja.

### 2.2 Database (MariaDB 10.3.39, `kapi_test_prod2`)
- ID berupa `char(36)` GUID yang dibuat aplikasi.
- PIN ada di `mst_h_employee_attendance.pin_id` (tanpa `company_id`, tanpa index pada `pin_id`); perusahaan di `mst_h_employee.company_id` (ber-index); karyawan `is_delete = 1` disembunyikan; PIN kosong tampil sebagai 0.
- `mst_a_device.device_sn` tidak unik secara struktur (saat ini tidak ada duplikat); `timezone` berformat `+07:00`; `is_active` default 0.
- Ada 3 grup `(company, pin)` dengan lebih dari satu karyawan aktif, dan ±15% baris attendance ber-PIN NULL/0.
- `mst_a_device_employee` adalah tabel alokasi karyawan → alat (`status` 1 = berhasil dikirim; arti `permission` belum diketahui, seluruh baris bernilai 1). Pada salinan database ini: 76 baris, 76 karyawan, hanya 1 alat, tanpa baris yatim; setiap baris tertaut satu lawan satu ke `mst_a_device_cmd_log.id`. Penyebaran antar alat belum pernah terjadi pada data ini sehingga hanya dapat diuji dengan data sintetis.
- Tidak ada trigger. `mst_a_device_cmd_log.trx_id` NOT NULL tanpa auto_increment dan praktis tidak dipakai (nilai 0–2 pada 143 baris); korelasi memakai `id`.
- `trx_a_presence` tidak punya unique key; index `idx_pr_employee_date (employee_id, presence_date)` tersedia.
- Tabel template tidak punya kolom jenis / versi. **Migrasi 001 sudah dijalankan**: kolom `type`, `major_ver`, `minor_ver` (`INT NULL`) di `mst_a_employee_att_temp_face` dan `mst_a_employee_att_temp_palm`. Baris lama bernilai NULL = legacy.

### 2.3 Perilaku logger lama (C# dan port Python sama)
- `presence_date` dikirim sampai menit (`yyyy-MM-dd HH:mm`), `type_device = "Device"`, `flag = kolom Verify` (bukan Status).
- `table=BIODATA` hanya memproses Type 1, 2, 8; **Type 9 diabaikan**. Pada OPERLOG, `USERPIC` dan `BIOPHOTO` sama-sama menjadi `index=100, is_biodata=1`; baris `FACE` lama menjadi `is_biodata=0`; jari disimpan `fid+1` ke `fp1..fp10` dengan `vx = MajorVer` (9 bila template diawali "oco", selain itu 10, bila MajorVer kosong).
- Logger memanggil `UpdateUser`, `UpdateUserFP` tiap 1 detik dan `UpdateUserFace` / `UpdateUserPalm` per alat online; API memindahkan tabel sementara ke tabel final lalu membuat baris command untuk alat lain. Logika ini yang dipindahkan ke desain ini.
- Data lama: wajah `index` 0–14 (`is_biodata=1`, panjang 2688), foto `index=100`; palm `index` 0–5 (308–2688 karakter). Punch alat lama bernilai `flag` 1, 2, 3, 4, 15, 25 (tidak pernah 0).

## 3. Arsitektur

```
alat ──HTTP──> zkadms (library, tanpa dependensi)
                  │ callback berurutan (satu worker), 503 bila penuh
                  ▼
              kapi/ (modul Go terpisah, go-sql-driver/mysql)
                  ├── sync    : logika callback
                  ├── store   : antarmuka Store + implementasi MySQL
                  └── cmdpoll : polling antrian command, status alat
                                   │
                                   ▼
                          MariaDB (kapi_test_prod2 / produksi)
```

Root `go.mod` tetap tanpa dependensi. Modul `kapi/` memiliki `go.mod` sendiri dengan `replace` ke library, seperti `examples/database`. Linter, `deadcode`, dan `govulncheck` tidak mencakup modul terpisah; penambahan ke `make lint` / CI termasuk dalam tahap 6.

## 4. Perubahan library (`zkadms`)

Netral vendor, tanpa istilah KAPI.

- `table=OPERLOG` dipecah per baris: `USER` → `UserRecord` (tipe yang ada); `USERPIC` dan `BIOPHOTO` → tipe baru `UserPhoto{PIN, FileName, Type, Size, Content}` (`Content` tetap string base64, tidak di-decode); baris `OPLOG` diabaikan.
- `table=BIODATA` → tipe baru `BioTemplate{PIN, No, Index, Valid, Duress, Type, MajorVer, MinorVer, Format, Data}`. **Semua `Type` diteruskan**; memilih jenis yang disimpan adalah tugas konsumen.
- Callback baru dengan bentuk opsi fungsional yang sama: `WithOnOperLog(fn(ctx, sn, OperLogBatch))` (user dan foto) dan `WithOnBioData(fn(ctx, sn, []BioTemplate))`. Keduanya memakai jalur dispatch yang ada (berurutan, 503 bila antrian penuh).
- Tes memakai fixture sintetis berbentuk sama dengan hasil capture, tanpa biometrik asli. Bentuk kiriman nyata ZAM230 dicatat di `datasheet/ADMS.md`. Semua fungsi publik baru dipanggil oleh tes agar lolos gerbang `deadcode`.
- Tidak berubah: endpoint yang ada, `ATTLOG`, antrian command.

## 5. Modul KAPI

### 5.1 Konfigurasi dan keamanan
- DSN database hanya dari environment (`KAPI_DSN`); alamat listen, interval polling, dan ambang online lewat flag / env. Tidak ada kredensial di repo.
- Tidak pernah mencatat isi template, foto, password, atau nomor kartu di log; log hanya memuat SN, PIN, dan ukuran.
- Hak akses database yang dibutuhkan (user khusus, bukan `devops`): `SELECT` pada `mst_a_device`, `mst_h_employee`, `mst_h_employee_attendance`, `mst_a_device_employee`; `INSERT` pada `trx_a_presence`, `trx_a_process_attendance`; `INSERT, UPDATE` pada `mst_a_employee_att_temp_face`, `_palm`, `_fp`, `mst_a_device_cmd_log`; `UPDATE` pada `mst_a_device`, `mst_h_employee_attendance`; `UPDATE, DELETE` pada `mst_a_device_employee`.

### 5.2 Penyelesaian identitas (aturan tenancy)
Setiap callback dimulai dengan:
1. **SN → alat.** Cari di `mst_a_device`. SN yang tidak ada, `is_active = 0`, atau cocok dengan lebih dari satu baris ditolak dan dicatat (data dibuang tanpa error ke alat, agar tidak dikirim ulang tanpa henti). Hasil (`device_id`, `company_id`, `timezone`) di-cache dengan masa berlaku pendek.
2. **PIN → karyawan.** `mst_h_employee e JOIN mst_h_employee_attendance a` dengan `e.company_id = ?`, `a.pin_id = ?`, `IFNULL(e.is_delete,0) = 0`. PIN 0 atau NULL ditolak. Lebih dari satu kecocokan ditolak dan dicatat; tidak pernah menebak.

PIN tidak pernah dipakai tanpa SN → company.

**PIN yang belum punya karyawan di KAPI (keputusan: perilaku lama dipertahankan).** User, template, foto, dan absensi dari PIN seperti itu dibuang; server hanya mencatat satu baris log (SN, PIN, jenis data, waktu; tanpa isi biometrik). Alat membalas OK sehingga umumnya tidak mengirim ulang. Data di alat tetap ada. Pemulihan dilakukan manual setelah karyawan didaftarkan di web: aksi "Download User" (`DATA QUERY USERINFO`, atau `DATA QUERY USERINFO PIN=n` untuk satu PIN) untuk user dan template, serta `DATA QUERY ATTLOG` dengan rentang waktu untuk absensi. Hasil query itu masuk lewat jalur biasa. Penampung otomatis (tabel `mst_a_device_unmatched` dan penyapu) sengaja tidak dibuat; dapat ditambahkan kemudian tanpa mengubah desain lain.

### 5.3 Jalur tulis (alat → DB)

**Absensi.** Satu transaksi:
- `trx_a_presence`: `presence_id` GUID, `presence_date` = jam dinding alat dipotong ke menit (format string, tanpa konversi zona oleh driver; zona dari `mst_a_device.timezone`, lalu default), `company_id`, `employee_id`, `pin_id`, `flag`, `type_device = 'Device'`, `device_name = SN`, `created_by = 'Logger'`, `created_date = now`, `approved_status_id` = konstanta auto-approved yang sama dengan API lama.
- Lewati bila sudah ada baris `(employee_id, presence_date menit yang sama)` (pencegah duplikat; satu penulis per database).
- Lalu `trx_a_process_attendance` untuk hari itu (`first_date` 00:00:00, `end_date` 23:59:59, `period` `MMM yyyy`, `created_by 'Auto Process'`, `updated_by 'Push Logger'`) agar API KAPI menghitungnya.
- `flag`: `Status` bila 0–5; bila 255, kode verifikasi (perilaku lama).

**User.** Memperbarui `card_no` (`[0000000000]` → `0`), `password_device`, dan `priv` pada `mst_h_employee_attendance` **hanya untuk karyawan yang sudah ada** (PIN tak dikenal dibuang), lalu menyebarkan "Send User" (lihat 5.5). Nama untuk command diambil dari KAPI, bukan dari alat (nama dari alat sering kosong).

**Template.** Upsert langsung ke tabel final (tanpa tabel sementara):

| Sumber | Tabel / kunci | Catatan |
|---|---|---|
| BIODATA Type 2 | face, `(employee, index, is_biodata=1)`, `type=2` | wajah NIR |
| BIODATA Type 9 | face, `(employee, index, is_biodata=1)`, `type=9` | wajah cahaya tampak; `major_ver`, `minor_ver` terisi |
| BIODATA Type 8 | palm, `(employee, index)`, `type=8` | |
| BIODATA Type 1 | fp, `(employee, vx)`, slot `No+1` → `fp1..fp10` | `vx = MajorVer`; belum teruji di alat (alat uji tanpa sensor jari) |
| USERPIC / BIOPHOTO | face, `index=100, is_biodata=1`, `type` NULL | satu baris; `size` = panjang konten |
| Type lain | diabaikan, dicatat di log | |

Kunci pencarian menyertakan jenis efektif: baris `type` NULL dengan `index < 100` dianggap `type=2` (wajah) atau `type=8` (palm), sehingga baris lama tidak tertimpa template jenis lain di `index` yang sama. `size` template = 0; `size` foto = panjang konten. Penghitung `total_face`, `total_palm`, `total_fp9/10/12` di `mst_h_employee_attendance` dihitung ulang dengan makna lama (`total_face` / `total_palm` = jumlah baris karyawan itu di tabelnya).

### 5.4 Jalur command (DB → alat)
- Poller (default tiap 2 detik) mengambil, untuk alat yang sedang online menurut library: `SELECT id, serial_number, content, pin_id, action FROM mst_a_device_cmd_log WHERE serial_number IN (...) AND transaction_time IS NULL AND return_value IS NULL ORDER BY commite_time, id LIMIT 100`.
- **Claim** setiap baris dengan `UPDATE ... SET transaction_time = NOW() WHERE id = ? AND transaction_time IS NULL`; hanya bila 1 baris berubah, command dimasukkan ke antrian library. `content` divalidasi (tanpa CR/LF) oleh library.
- ID command library dipetakan di memori ke `cmd_log.id`. Saat hasil datang: `UPDATE return_value, respone_time, message`; sukses bila `Return >= 0`. Efek samping yang dipertahankan: bila `cmd_log.id` sama dengan sebuah `mst_a_device_employee.device_employee_id` (web KAPI membuat baris alokasi dan baris command dengan ID yang sama), sukses memperbarui `status = 1` pada baris alokasi itu; command tanpa tautan (hasil penyebaran kita) tidak menyentuh tabel alokasi; balasan `INFO` → `device_name_info` pada `mst_a_device`; sukses pada aksi `Device Clear User` → hapus baris `mst_a_device_employee` alat itu.
- Baris yang sudah di-claim tetapi tidak dibalas dalam 10 menit (mis. server restart) dikembalikan ke antrian (`transaction_time = NULL`) paling banyak 3 kali; setelahnya `return_value = 'Timeout'`. Command bersifat idempoten (UPDATE / DELETE).
- Pengiriman tidak bergantung pada kolom `status`.

### 5.5 Penyebaran ke alat lain
Saat user atau template masuk dari alat A: cari alat B lain dalam perusahaan yang sama yang terhubung ke karyawan itu (`mst_a_device_employee`, SN ≠ A). Untuk tiap B, buat baris `mst_a_device_cmd_log` (`type_log 'Auto Update'`, `action` `Send User` / `Send Finger N` / `Send Face i` / `Send Palm i`, `pin_id`, `content`, `commite_time`, `created_by 'Auto Update'`, `trx_id = 0` seperti baris yang ada).
- "Send User" dilewati bila sudah ada baris `(serial_number, type_log 'Auto Update', content)` yang sama (perilaku lama).
- **Aturan kompatibilitas template:** baris legacy (`type` NULL) dikirim persis seperti API lama (`Type=2/MajorVer=12` wajah, `Type=8/MajorVer=12` palm). Baris baru (`type` terisi) hanya dikirim ke alat dengan **model sama**, yaitu firmware tanpa akhiran `-Ver…` (mis. `ZAM230-NF50VA`); alat lain dilewati dan dicatat di log.
- Pengiriman yang dibuat web KAPI sendiri (`type_log 'Device Employee'`) tetap tanggung jawab KAPI; batasannya: kode itu memakai `Type=2/MajorVer=12`, tidak cocok untuk template ZAM230.

### 5.6 Status alat
`mst_a_device.status` mencerminkan keadaan nyata: **0** offline, **1** online tanpa antrian, **2** online dengan antrian.
- Online = ada request dari alat dalam ambang (`WithOnlineThreshold`, default 2 menit).
- Antrian = baris `cmd_log` alat itu yang belum terkirim + command yang sudah di-claim tetapi belum dibalas.
- Ditulis hanya saat **berpindah keadaan**, ditambah penyegaran `description` paling sering tiap beberapa detik. Deskripsi: `Connect`, `Not Connect`, `Process Sync (N menunggu)`. Saat online: `device_ip`, `firmware` ikut diperbarui; saat berpindah ke online dari offline juga `updated_date`.
- Penyapu berkala (default 30 detik) menandai 0 alat milik proses ini yang melewati ambang; saat server berhenti, semua alat milik proses ini ditandai 0. **Hanya SN yang pernah dilayani proses ini yang disentuh** (tidak seperti `UpdateAllDeviceOff` yang mematikan semua alat di luar daftar pemanggil).

### 5.7 Kegagalan
- Error sementara (koneksi putus, `lock wait timeout`, deadlock, "gone away") diulang dengan backoff eksponensial (1 detik sampai 30 detik) **di dalam callback**. Worker callback tunggal terblokir, antrian library penuh, library membalas 503, alat menyimpan data dan mengirim ulang. Tidak ada data hilang.
- Error permanen (data tidak valid, pelanggaran constraint) dicatat tanpa isi biometrik dan dilewati, agar satu data buruk tidak menahan yang lain.
- Semua penulisan idempoten terhadap pengiriman ulang.

## 6. Pengujian
- **Library:** parser dengan fixture sintetis; tes batas (baris cacat, konten besar, OPERLOG campuran).
- **Modul:** `Store` palsu untuk logika; tes wajib: **dua perusahaan dengan PIN sama**, kiriman ulang (idempoten), database mati lalu pulih (backoff dan 503), PIN ambigu / 0 / tidak dikenal, SN tidak aktif / ganda, serta transisi status 0/1/2.
- **Integrasi:** MariaDB 10.3 sementara (Docker, bila tersedia) dengan skema hasil ekspor, ditandai build tag `integration` dan `KAPI_TEST_DSN`.
- **Penerimaan:** alat ZAM230 dengan PIN uji terhadap **database uji lokal**. Penulisan ke `kapi_test_prod2` hanya atas izin tegas pemilik proyek.

## 7. Urutan implementasi
0. Library: parser dan callback `OPERLOG` / `BIODATA`, dokumentasi.
1. Kerangka modul: konfigurasi, `Store`, penyelesaian SN → company → karyawan, status online/offline.
2. Absensi, diverifikasi di alat.
3. Penerimaan user dan template ke tabel final, plus penghitung.
4. Penyebaran ke alat lain dengan aturan kompatibilitas.
5. Polling command, pencatatan hasil, status "Process Sync".
6. Pengerasan: log, shutdown rapi, dokumentasi, `make lint` / CI untuk modul `kapi/`.

## 8. Peralihan ke produksi dan rollback
Per alat: arahkan Cloud Server Setting alat ke server baru; alat lain tetap di logger lama. Dua logger tidak boleh melayani alat yang sama. Selama logger lama masih hidup, panggilan `UpdateAllDeviceOff`-nya dapat menandai alat kita sebagai offline; matikan logger lama setelah semua alat pindah. Rollback = mengembalikan setting alat.

## 9. Keputusan yang perlu dikonfirmasi saat tinjauan
1. Efek samping lama "hapus `mst_a_device_employee` bila command gagal" **tidak dipindahkan** (hanya dicatat sebagai gagal), karena merusak penugasan hanya karena satu kegagalan sementara.
2. Kebijakan batas waktu command (10 menit, 3 percobaan, lalu `Timeout`).
3. Migrasi opsional 002: index `(serial_number, transaction_time)` pada `mst_a_device_cmd_log` (tidak ada index pada `serial_number` sekarang).
4. Konstanta `approved_status_id` (auto-approved) disalin dari API lama.
5. Jalur jari mengikuti aturan logger lama tetapi belum teruji di perangkat keras.
6. Bagian 5.5: baris `Device Employee` buatan web KAPI tetap memakai `Type=2/MajorVer=12`.
7. User yang didaftarkan langsung di alat tidak otomatis mendapat baris `mst_a_device_employee` untuk alat itu (perilaku lama dipertahankan). Ubah bila alat tempat mendaftar harus ikut tercatat sebagai alokasi.
