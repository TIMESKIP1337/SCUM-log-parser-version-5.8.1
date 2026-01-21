# Logs Parser Bot v10.0.1

Discord Bot for SCUM Game Server Monitoring with Log Parsing and Memory Management

Discord Bot สำหรับ SCUM Game Server Monitoring พร้อมระบบ Log Parsing และ Memory Management

---

## Quick Start / เริ่มต้นใช้งาน

Use `/setup-logs` command to automatically create channels (Admin only) and then restart the bot 

ใช้คำสั่ง `/setup-logs` สำหรับสร้าง channel แบบอัตโนมัติ (เฉพาะ Admin) หลังจากนั้นรีบอทด้วยครับ

---

## Features / ฟีเจอร์

| English | ไทย |
|---------|-----|
| **Log Parsing**: Parse SCUM game server logs (kills, economy, logins, etc.) | **Log Parsing**: แยกวิเคราะห์ log จาก SCUM server (kills, economy, logins ฯลฯ) |
| **Discord Integration**: Post formatted messages to Discord channels | **Discord Integration**: ส่งข้อความไปยัง Discord channels |
| **Ticket System**: Support ticket management | **Ticket System**: ระบบจัดการ ticket |
| **Channel Management**: Show/hide channels, welcome messages | **Channel Management**: ซ่อน/แสดง channels, ข้อความต้อนรับ |
| **Advanced Caching**: LRU cache with 50k entries | **Advanced Caching**: LRU cache รองรับ 50k entries |
| **Memory Management**: Auto GC with monitoring | **Memory Management**: Auto GC พร้อมการ monitor |
| **Remote Access**: FTP/SFTP support for remote log files | **Remote Access**: รองรับ FTP/SFTP สำหรับไฟล์ log ระยะไกล |
| **UTF-8 Support**: Full international character support | **UTF-8 Support**: รองรับตัวอักษรนานาชาติ |

---

## Project Structure / โครงสร้างโปรเจค

```
Logs-version-8.0.0/
├── cmd/logs-bot/              # New entry point
│   └── main.go
├── internal/                  # Refactored packages
│   ├── config/               # Configuration management
│   ├── shared/               # Common utilities
│   ├── infrastructure/       # External systems (cache, batch, memory, remote)
│   └── domain/            
├── main.go                   # Original entry point 
├── *.go                      # Original modules 
├── go.mod
└── .env
```

---

## How to Run / วิธีการรัน

### Option 1: New Structure (Recommended) / โครงสร้างใหม่ (แนะนำ)

```bash
# Build
go build -o logs-bot ./cmd/logs-bot

# Run
./logs-bot
```

Or run directly / หรือรันโดยตรง:
```bash
go run ./cmd/logs-bot
```

### Option 2: Legacy Structure / โครงสร้างเดิม

```bash
# Build
go build -o logs-bot

# Run
./logs-bot
```

Or run directly / หรือรันโดยตรง:
```bash
go run .
```

---

## Configuration / การตั้งค่า

Create `.env` file / สร้างไฟล์ `.env`:

```env
# Discord Configuration
DISCORD_TOKEN=your_discord_bot_token_here

# Channel IDs
KILL_CHANNEL_ID=1234567890
LOGIN_CHANNEL_ID=1234567890
ECONOMY_CHANNEL_ID=1234567890
CHAT_CHANNEL_ID=1234567890
ADMIN_CHANNEL_ID=1234567890
TICKET_CHANNEL_ID=1234567890

# Local Configuration
LOCAL_LOGS_PATH=./logs/
ENABLE_FILE_WATCHER=true

# Remote Configuration (Optional) เปลื่ยนเป็น FTP / SFTP 
PROTOCOL=SFTP
FTP_HOST=example.com
FTP_PORT=22
FTP_USER=username
FTP_PASSWORD=password
LOGS_PATH=/path/to/logs/

# Performance Settings
CACHE_CAPACITY=50000
MAX_MEMORY_MB=2048
MAX_GOROUTINES=32
CHECK_INTERVAL=30
```

---

## Architecture / สถาปัตยกรรม

### Clean Architecture Layers

```
┌─────────────────────────────────────┐
│         cmd/logs-bot (main)         │  <- Entry point
├─────────────────────────────────────┤
│          internal/config            │  <- Configuration
├─────────────────────────────────────┤
│        internal/domain/*            │  <- Business Logic
│  (parser, discord modules)          │
├─────────────────────────────────────┤
│     internal/infrastructure/*       │  <- External Systems
│  (cache, batch, memory, remote)     │
├─────────────────────────────────────┤
│        internal/shared              │  <- Common Utilities
└─────────────────────────────────────┘
```

### Package Responsibilities / หน้าที่ของแต่ละ Package

#### `internal/config`
- Configuration loading and validation / โหลดและตรวจสอบ configuration
- Environment variable management / จัดการ environment variables
- UTF-8 environment setup / ตั้งค่า UTF-8

#### `internal/infrastructure`
- **cache**: LRU cache implementation / ระบบ LRU cache
- **batch**: Batch message sender / ส่งข้อความแบบ batch
- **memory**: Memory management and pooling / จัดการหน่วยความจำ
- **remote**: FTP/SFTP connection handling / จัดการการเชื่อมต่อ FTP/SFTP

#### `internal/domain`
- **parser**: Log parsing logic (killfeed, logs, economy) / logic สำหรับ parse log
- **discord**: Discord features (admin, chat, tickets) / ฟีเจอร์ Discord

#### `internal/shared`
- Helper functions (StringPtr, IntPtr, Min, etc.) / ฟังก์ชันช่วยเหลือ
- File utilities / เครื่องมือจัดการไฟล์

---

## Development / การพัฒนา

### Prerequisites / สิ่งที่ต้องมี

- Go 1.24.4 or higher / Go 1.24.4 ขึ้นไป
- Discord Bot Token
- SCUM Server logs (local or remote) / log จาก SCUM Server (local หรือ remote)

### Building / การ Build

```bash
# Build new version / Build เวอร์ชันใหม่
go build -o logs-bot-new ./cmd/logs-bot

# Build legacy version / Build เวอร์ชันเดิม
go build -o logs-bot .
```

### Testing / การทดสอบ

```bash
# Test imports
go build ./...

# Verify packages
go list ./...
```

---

## Performance Features / ฟีเจอร์ด้านประสิทธิภาพ

### Memory Management / การจัดการหน่วยความจำ
- **Aggressive GC**: 50% trigger threshold
- **Max Memory**: Configurable limit (default: 2048 MB) / กำหนดได้ (ค่าเริ่มต้น: 2048 MB)
- **Object Pooling**: Reusable buffers and objects / ใช้ buffers และ objects ซ้ำได้

### Caching / ระบบ Cache
- **LRU Cache**: 50,000 entries default / ค่าเริ่มต้น 50,000 entries
- **Persistence**: Auto-save every 5 minutes / บันทึกอัตโนมัติทุก 5 นาที
- **Cleanup**: Remove entries older than 7 days / ลบ entries ที่เก่ากว่า 7 วัน

### Concurrency / การทำงานพร้อมกัน
- **Goroutine Limiting**: Max configurable goroutines / กำหนดจำนวน goroutines ได้
- **Panic Recovery**: Safe goroutine wrappers / ป้องกัน panic
- **Graceful Shutdown**: 30-second timeout

---

## UTF-8 Support / รองรับ UTF-8

Full support for international characters / รองรับตัวอักษรนานาชาติ:
- Thai / ภาษาไทย
- Chinese / ภาษาจีน
- All Unicode characters / ตัวอักษร Unicode ทั้งหมด

---

## Graceful Shutdown / การปิดอย่างปลอดภัย

Press `Ctrl+C` to trigger graceful shutdown / กด `Ctrl+C` เพื่อปิด:

1. Stop accepting new requests / หยุดรับ requests ใหม่
2. Flush pending messages / ส่งข้อความที่ค้างอยู่
3. Save cache to disk / บันทึก cache
4. Close connections / ปิดการเชื่อมต่อ
5. Run final GC / รัน GC ครั้งสุดท้าย


### Runtime Errors / ปัญหาขณะรัน

- **403 Forbidden**: Check Discord token validity / ตรวจสอบ token
- **Cannot connect**: Verify network connectivity / ตรวจสอบการเชื่อมต่อ
- **Cache errors**: Check file permissions / ตรวจสอบ permissions

---

## License / ลิขสิทธิ์

This project is proprietary software for SCUM game server monitoring.

ซอฟต์แวร์นี้เป็นกรรมสิทธิ์สำหรับการ monitor SCUM game server

---

## Author / ผู้พัฒนา

TIMESKIP1337

---

**Version**: 8.0.0
**Last Updated**: 2025-11-18
**Status**: Production Ready (Legacy)
