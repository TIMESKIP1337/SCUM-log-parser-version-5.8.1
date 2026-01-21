# Refactoring Documentation - Logs Parser v8.0.0

## 📁 โครงสร้างใหม่ (New Structure)

โปรเจกต์ได้ถูก refactor แล้วตามหลัก **Clean Architecture** โดยแยก packages ออกเป็น layers ดังนี้:

```
Logs-version-8.0.0/
├── cmd/
│   └── logs-bot/           # Main entry point (อยู่ระหว่างการพัฒนา)
│       └── main.go
├── internal/
│   ├── config/             # Configuration management
│   │   └── config.go       # ✅ Config struct, environment variables, UTF-8 setup
│   ├── shared/             # Shared utilities
│   │   └── helpers.go      # ✅ Helper functions (StringPtr, IntPtr, Min, etc.)
│   ├── infrastructure/     # Infrastructure layer (external concerns)
│   │   ├── cache/
│   │   │   └── iru_cache.go           # ✅ LRU cache implementation
│   │   ├── batch/
│   │   │   └── batch_sender.go        # ✅ Batch message sender
│   │   ├── memory/
│   │   │   ├── memory_manager.go      # ✅ Memory monitoring & GC management
│   │   │   ├── memory_pool.go         # ✅ Object pooling
│   │   │   └── stream_processor.go    # ✅ Large file streaming
│   │   └── remote/
│   │       ├── remote_connection.go   # ✅ Remote connection interface
│   │       ├── ftp_client.go          # ✅ FTP client pool
│   │       └── sftp_pool.go           # ✅ SFTP client pool
│   └── domain/             # Domain layer (business logic)
│       ├── parser/         # Log parsing logic
│       │   ├── killfeed.go        # ✅ Kill feed parser
│       │   ├── logs.go            # ✅ Economy/login/kill count parser
│       │   ├── logs_optimized.go  # ✅ Optimized parsers
│       │   └── logsetc.go         # ✅ Extended logs parser
│       └── discord/        # Discord-specific features
│           ├── admin.go       # ✅ Admin commands
│           ├── chat.go        # ✅ Chat relay
│           ├── lockpick.go    # ✅ Lockpicking stats
│           ├── ticket.go      # ✅ Support ticket system
│           ├── showhide.go    # ✅ Channel visibility
│           └── welcome.go     # ✅ Welcome/goodbye messages
├── go.mod                  # ✅ Updated module path
└── main.go                 # ⚠️ Original file (will be deprecated)
```

## 🎯 การเปลี่ยนแปลงหลัก (Major Changes)

### 1. **Package Separation**
- **Before**: ทุกอย่างอยู่ใน `package main` (21 files in root)
- **After**: แยกเป็น packages ตาม layers (config, shared, infrastructure, domain)

### 2. **Module Path**
```go
// Before
module discord-bot-unified

// After
module github.com/TIMESKIP1337/Logs-version-8.0.0
```

### 3. **Import Paths**
ตัวอย่าง imports ที่ต้องแก้ไข:

```go
// Before (ในไฟล์ root package)
import (
    "github.com/bwmarrin/discordgo"
)

// After (ใช้ในไฟล์อื่น)
import (
    "github.com/TIMESKIP1337/Logs-version-8.0.0/internal/config"
    "github.com/TIMESKIP1337/Logs-version-8.0.0/internal/infrastructure/cache"
    "github.com/TIMESKIP1337/Logs-version-8.0.0/internal/infrastructure/batch"
    "github.com/TIMESKIP1337/Logs-version-8.0.0/internal/infrastructure/memory"
    "github.com/TIMESKIP1337/Logs-version-8.0.0/internal/infrastructure/remote"
    "github.com/TIMESKIP1337/Logs-version-8.0.0/internal/domain/parser"
    "github.com/TIMESKIP1337/Logs-version-8.0.0/internal/domain/discord"
    "github.com/TIMESKIP1337/Logs-version-8.0.0/internal/shared"

    "github.com/bwmarrin/discordgo"
)
```

## 🔧 ขั้นตอนถัดไป (Next Steps)

### Phase 1: ปรับปรุง Imports (แนะนำให้ทำทีละ package)

1. **เริ่มจาก Infrastructure Layer**:
   ```bash
   # Update imports in infrastructure packages
   cd internal/infrastructure/cache
   # แก้ไข imports จาก main package เป็น relative imports
   ```

2. **แก้ไข Global Variables**:
   - ย้าย global variables ไปยัง package ที่เหมาะสม
   - ใช้ dependency injection แทน global state
   - สร้าง interfaces สำหรับ dependencies

3. **สร้าง Main Entry Point ใหม่**:
   ```go
   // cmd/logs-bot/main.go
   package main

   import (
       "github.com/TIMESKIP1337/Logs-version-8.0.0/internal/config"
       "github.com/TIMESKIP1337/Logs-version-8.0.0/internal/infrastructure/cache"
       // ... other imports
   )

   func main() {
       cfg := config.Load()
       // Initialize and wire up dependencies
   }
   ```

### Phase 2: Dependency Injection

ปัญหาหลักคือ global variables ที่ต้องแก้ไข:

```go
// ปัจจุบัน (ใน main.go เดิม)
var (
    SharedSession      *discordgo.Session
    Config             SharedConfig
    GlobalMemoryManager *MemoryManager
    SentLogsCache      *LRUCache
)

// แนะนำให้เปลี่ยนเป็น
type Application struct {
    Config         *config.Config
    Session        *discordgo.Session
    MemoryManager  *memory.Manager
    Cache          *cache.LRUCache
    BatchSender    *batch.Sender
    // ... other dependencies
}
```

### Phase 3: Unit Tests

สร้าง tests สำหรับแต่ละ package:

```bash
# ตัวอย่าง test structure
internal/
  config/
    config_test.go
  infrastructure/
    cache/
      iru_cache_test.go
    batch/
      batch_sender_test.go
    memory/
      memory_manager_test.go
```

## 📊 Benefits ของโครงสร้างใหม่

### ✅ Separation of Concerns
- **Config**: จัดการ configuration แยกส่วน
- **Infrastructure**: External dependencies (cache, batch, memory, remote)
- **Domain**: Business logic (parsers, discord features)
- **Shared**: Common utilities

### ✅ Testability
- แยก packages ทำให้ง่ายต่อการเขียน unit tests
- Mock dependencies ได้ง่ายขึ้น

### ✅ Maintainability
- ค้นหาโค้ดได้เร็วขึ้น (รู้ว่าอยู่ที่ไหน)
- แก้ไขได้ง่ายขึ้น (ไม่กระทบส่วนอื่น)

### ✅ Scalability
- เพิ่ม features ใหม่ได้ง่าย
- แยก microservices ได้ในอนาคต

## ⚠️ Known Issues & TODOs

1. **Import Paths**: ยังต้องอัปเดต imports ในทุกไฟล์ที่ย้ายแล้ว
2. **Global Variables**: ต้อง refactor ให้ใช้ dependency injection
3. **Main Entry Point**: ต้องสร้าง `cmd/logs-bot/main.go` ใหม่
4. **Tests**: ยังไม่มี tests
5. **Documentation**: ต้องเพิ่ม documentation สำหรับแต่ละ package

## 🚀 คำแนะนำในการทำงานต่อ

### Option 1: Incremental Migration (แนะนำ)
1. เก็บ `main.go` เดิมไว้ใช้งานได้ต่อ
2. ค่อยๆ refactor ทีละ module
3. สร้าง tests ให้แต่ละ module
4. เมื่อเสร็จแล้วค่อยสลับไปใช้ `cmd/logs-bot/main.go`

### Option 2: Big Bang Migration (เสี่ยง)
1. แก้ไข imports ทุกไฟล์พร้อมกัน
2. แก้ไข global variables ทั้งหมด
3. สร้าง main.go ใหม่
4. Test ทั้งระบบ

## 📝 Example: การใช้งาน New Packages

```go
// Example: Using config package
import "github.com/TIMESKIP1337/Logs-version-8.0.0/internal/config"

cfg := config.Load()
fmt.Println(cfg.DiscordToken)
config.UpdateSharedActivity()

// Example: Using cache package
import "github.com/TIMESKIP1337/Logs-version-8.0.0/internal/infrastructure/cache"

lruCache := cache.NewLRUCache(50000)
lruCache.Add("key", "value")
exists := lruCache.Exists("key")

// Example: Using shared helpers
import "github.com/TIMESKIP1337/Logs-version-8.0.0/internal/shared"

namePtr := shared.StringPtr("John")
minValue := shared.Min(5, 10)
```

## 🎓 แนวคิดเพิ่มเติม

### Clean Architecture Layers

```
┌─────────────────────────────────────┐
│         cmd/logs-bot (main)         │  ← Entry point
├─────────────────────────────────────┤
│          internal/config            │  ← Configuration
├─────────────────────────────────────┤
│        internal/domain/*            │  ← Business Logic
│  (parser, discord modules)          │
├─────────────────────────────────────┤
│     internal/infrastructure/*       │  ← External Systems
│  (cache, batch, memory, remote)     │
├─────────────────────────────────────┤
│        internal/shared              │  ← Common Utilities
└─────────────────────────────────────┘
```

### Dependency Flow

```
main → config → infrastructure → domain → shared
  ↓       ↓            ↓            ↓
  ↓       ↓            ↓            ↓
  └───────────────────────────────────→ External Packages
                                        (discordgo, sqlite, etc.)
```

---

**สถานะ**: 🟡 In Progress
**คะแนนการ Refactor**: 60% เสร็จสมบูรณ์
- ✅ Directory structure created
- ✅ Files moved to appropriate locations
- ✅ Package names updated
- ✅ go.mod updated
- ⏳ Imports need updating
- ⏳ Global variables need refactoring
- ⏳ Main entry point needs creating
- ⏳ Tests need writing

**ผู้ดำเนินการ**: Claude Code
**วันที่**: 2025-11-18
