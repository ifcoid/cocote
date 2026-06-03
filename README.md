# cocote

**C**laude **Co**de Cloner Prompt MCP Server using **Te**legram.

MCP eksternal untuk menerima update, progress, balasan/tap, notifikasi selesai, atau approval interaktif (seperti meng-clone tampilan Claude Code yang sedang aktif) via Telegram.

cocote harus meng-konsumsi update Telegram (`getUpdates`). Telegram hanya boleh **satu konsumen per bot** — jadi kalau ada beberapa sesi yang polling bot yang sama, akan bentrok (HTTP 409). Solusinya: **booking**. Sesi Claude Code pertama yang start akan mem-booking cocote dan menjadi konsumen Telegram yang aktif; sesi lain otomatis jalan dalam mode *send-only*. Begitu sesi yang mem-booking selesai, lease dilepas dan sesi berikutnya bisa mengambil alih.

## Cara kerja booking

```
Sesi Claude Code A ──(stdio)── cocote [HOLD lease] ──getUpdates──▶ Telegram   (ACTIVE: notify + approval + reply + mirror)
Sesi Claude Code B ──(stdio)── cocote [send-only]  ──sendMessage─▶ Telegram   (SEND-ONLY: notify + mirror saja)
```

- Lease disimpan di `<user-config-dir>/cocote/booking.lock` berisi identitas pemilik + heartbeat.
- Pemegang lease mem-perbarui heartbeat berkala. Kalau prosesnya mati, heartbeat berhenti; setelah `COCOTE_LEASE_TTL` lewat, lease dianggap basi dan sesi lain boleh mengambil alih.
- `sendMessage` tidak pernah bentrok, jadi semua sesi tetap bisa kirim notifikasi. Yang eksklusif hanya `getUpdates` (menerima tap & balasan).

## MCP tools

| Tool | Butuh booking? | Fungsi |
|------|----------------|--------|
| `notify` | tidak (send-only ok) | Kirim progress / notifikasi selesai. Field: `message`, `level` (info/success/warning/error). |
| `ask_approval` | ya | Minta approval lewat tombol inline yang bisa di-tap; blok sampai dipilih atau timeout. Field: `question`, `options[]` (label tombol custom), `columns` (tombol per baris, default 2), `timeout_seconds`. |
| `wait_for_reply` | ya | Tanya bebas, tunggu balasan teks user. Field: `prompt`, `timeout_seconds`. |
| `mirror_screen` | tidak (send-only ok) | "Clone tampilan" — mirror layar/output Claude Code, meng-edit satu pesan di tempat agar terasa live. Field: `content`, `title`, `new`. |

## Setup

1. **Buat bot Telegram** lewat [@BotFather](https://t.me/BotFather), salin tokennya.
2. **Cari chat id**: kirim pesan ke bot, lalu buka `https://api.telegram.org/bot<TOKEN>/getUpdates` dan ambil `chat.id`.
3. **Konfigurasi**: salin `.env.example` ke `.env` dan isi `COCOTE_BOT_TOKEN` + `COCOTE_CHAT_ID`.
4. **Build**:
   ```sh
   go build -o cocote.exe .
   ```

## Daftarkan ke Claude Code

Server ini sudah terdaftar di [.mcp.json](.mcp.json) (scope project). Edit `command` agar menunjuk ke biner hasil build, lalu set kredensial via `.env` atau lewat field `env` di `.mcp.json`.

Atau daftarkan via CLI:

```sh
claude mcp add cocote -- /absolute/path/to/cocote.exe
```

Konfigurasi (`COCOTE_BOT_TOKEN`, `COCOTE_CHAT_ID`, dst.) dibaca dari environment variable, lalu fallback ke `.env` di working directory atau di sebelah biner. Lihat [.env.example](.env.example) untuk semua opsi.

## Auto-mirror (otomatis tiap giliran selesai)

`mirror_screen` di atas dipanggil **manual** oleh model. Untuk meng-clone tampilan **otomatis** tiap Claude Code selesai menjawab, cocote menyediakan subcommand `cocote mirror` yang dipasang ke **hook `Stop`** Claude Code.

Alurnya:

```
Claude Code selesai 1 giliran ──▶ hook Stop ──▶ `cocote.exe mirror` (stdin = payload hook) ──▶ edit/kirim pesan Telegram
```

`cocote mirror`:
- membaca payload hook dari stdin (berisi `transcript_path` + `session_id`),
- mengambil pesan **asisten terakhir** dari transcript,
- meng-**edit satu pesan Telegram per sesi** di tempat (live), atau mengirim baru kalau belum ada,
- bersifat *send-only* (tidak butuh booking) dan **selalu exit 0** supaya tidak pernah memblok sesi.

Hook-nya sudah terdaftar di [.claude/settings.json](.claude/settings.json) (scope project):

```json
{
  "hooks": {
    "Stop": [
      { "hooks": [ { "type": "command", "command": "\"<path>\\cocote.exe\" mirror" } ] }
    ]
  }
}
```

Sesuaikan `<path>` ke lokasi biner hasil build. Saat pertama kali jalan, Claude Code akan meminta persetujuan menjalankan hook project ini. Id pesan mirror disimpan per sesi di `mirror-<session>.id` dalam `COCOTE_LEASE_DIR`.

### Mirror per-tool (hook `PostToolUse`)

Untuk detail lebih dalam, ada subcommand `cocote tool` yang dipasang ke hook **`PostToolUse`**: tiap kali sebuah tool selesai dijalankan, ia mengirim satu baris ringkas ke Telegram, mis. `🔧 Bash · <session>` + perintahnya, atau `🔧 Edit · <session>` + path file. Indikator `❌` ditambahkan kalau tool-nya error.

Hook ini juga sudah terdaftar di [.claude/settings.json](.claude/settings.json) dengan `matcher` dibatasi ke tool yang berdampak (`Bash|Edit|Write|MultiEdit|NotebookEdit`) supaya tidak terlalu berisik:

```json
"PostToolUse": [
  {
    "matcher": "Bash|Edit|Write|MultiEdit|NotebookEdit",
    "hooks": [ { "type": "command", "command": "\"<path>\\cocote.exe\" tool" } ]
  }
]
```

Lebarkan `matcher` (mis. `.*` untuk semua tool) kalau mau mirror lebih lengkap, atau hapus entri ini kalau hanya butuh ringkasan `Stop`.

## Konfigurasi

| Variabel | Wajib | Default | Keterangan |
|----------|-------|---------|------------|
| `COCOTE_BOT_TOKEN` | ✅ | — | Token bot dari @BotFather. |
| `COCOTE_CHAT_ID` | ✅ | — | Chat id numerik tujuan. |
| `COCOTE_SESSION_NAME` | | `<host>-<pid>` | Label sesi di pesan Telegram. |
| `COCOTE_LEASE_TTL` | | `30s` | Umur lease tanpa heartbeat sebelum bisa diambil alih. |
| `COCOTE_POLL_TIMEOUT` | | `30s` | Timeout long-poll `getUpdates`. |
| `COCOTE_LEASE_DIR` | | `<user-config-dir>/cocote` | Lokasi file lock booking. |
| `COCOTE_ENV_FILE` | | — | Path eksplisit ke file env. |

## Struktur

```
main.go                     entrypoint: load config, booking, jalankan MCP server di stdio
internal/config/            loader env + .env
internal/booking/           lease lock + heartbeat (mekanisme booking)
internal/telegram/          klien Bot API, dispatcher, long-poller
internal/server/            wiring MCP tools (notify, ask_approval, wait_for_reply, mirror_screen)
internal/hook/              subcommand `cocote mirror` (hook Stop) & `cocote tool` (hook PostToolUse)
```

## Test

```sh
go test ./...
```

Dibangun dengan SDK MCP resmi [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk).
