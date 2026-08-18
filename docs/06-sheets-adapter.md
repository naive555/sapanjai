# Sapanjai — Google Sheets/Drive Adapter Spec (v0.1 draft)

> สถานะ: draft สำหรับใช้เป็น input ให้ Claude Code prompt
> ขอบเขต: **read-only MVP** — write tools อยู่ใน §9 (ยังไม่ build)

---

## 1. Scope & assumptions

- Connector `type` = `google_sheets` (ปัจจุบัน skeleton มีแค่ `"generic"`)
- Adapter เดียวครอบทั้ง Sheets + Drive — OAuth scope เดียวกัน, ผูกกันโดยธรรมชาติ (รูปใน Drive, link ใน Sheet)
- Data scale เป้าหมาย: **~87GB / spreadsheet หลายสิบไฟล์** → pagination + result cap เป็น requirement ไม่ใช่ nice-to-have
- ผู้ใช้ปลายทางคือ AI agent (Claude Desktop/Code, Cursor) ผ่าน MCP ไม่ใช่มนุษย์เรียกตรง

---

## 2. Endpoint & transport

```
POST /mcp/:connectorId        Streamable HTTP, stateless JSON
```

- **Streamable HTTP** ไม่ใช่ stdio — เพราะเป็น remote multi-tenant gateway (ตรงกับ best practice: stdio ไว้สำหรับ local single-user)
- Stateless JSON (ไม่ทำ session/SSE) — scale ง่ายกว่า, เข้ากับ k8s deployment ที่มีอยู่
- `connectorId` อยู่ใน path เพื่อให้ 1 org ที่มีหลาย Google account แยก endpoint กันได้

### ⚠️ ช่องว่างที่ต้องตัดสินใจก่อน: MCP client จะ auth ยังไง

Access token ปัจจุบันอายุ 15 นาที — MCP client (Claude Desktop) ไม่มีทาง refresh ให้เองได้ และไม่มี browser flow

ทางเลือก:

| ทางเลือก                                                                                                   | ข้อดี                                                   | ข้อเสีย                                                                          |
| ---------------------------------------------------------------------------------------------------------- | ------------------------------------------------------- | -------------------------------------------------------------------------------- |
| **A. Personal Access Token (PAT)** — table ใหม่ `api_keys`, hash เก็บ, `Authorization: Bearer sk_live_...` | ง่ายสุด, ตรงกับ mental model ของ MCP config, revoke ได้ | ต้องเพิ่ม module + migration ใหม่                                                |
| **B. OAuth 2.1 ตาม MCP spec**                                                                              | มาตรฐาน, client รองรับ native                           | งานเยอะกว่ามาก, ต้องทำ authorization server                                      |
| **C. ยืด refresh token มาใช้ตรงๆ**                                                                         | ไม่ต้องเพิ่มอะไร                                        | ผิด security model ที่วางไว้ (refresh token ควรอายุสั้น + rotate) — **ไม่แนะนำ** |

**เสนอ: A สำหรับ MVP** — 1 migration + 1 module เล็ก, PAT ผูกกับ org + set ของ permission (subset ของ role ผู้สร้าง)

---

## 3. Connector config shape (`google_sheets`)

เก็บใน `encrypted_config` ผ่าน envelope encryption เดิม — **ไม่มี field ไหนโผล่ใน response DTO หรือ log**

```json
{
  "oauth": {
    "refresh_token": "1//0g...",
    "client_id": "...apps.googleusercontent.com",
    "client_secret": "..."
  },
  "scope": {
    "spreadsheet_ids": ["1AbC...", "1XyZ..."],
    "drive_folder_ids": ["0B1a..."]
  }
}
```

**`scope` คือ security boundary ที่สำคัญที่สุด** — adapter ต้อง**ปฏิเสธ** spreadsheet/folder ที่ไม่อยู่ใน allowlist เสมอ แม้ OAuth token จะเข้าถึงได้ก็ตาม ไม่งั้น agent ที่หลุด prompt injection มาจะอ่าน Drive ทั้งบัญชีได้

---

## 4. Tool catalog

Naming: `{service}_{action}_{resource}` snake_case ตาม MCP convention

| Tool                          | Permission    | readOnly | สรุป                                             |
| ----------------------------- | ------------- | :------: | ------------------------------------------------ |
| `sheets_list_spreadsheets`    | `sheets:read` |    ✅    | list spreadsheet ที่อยู่ใน allowlist             |
| `sheets_describe_spreadsheet` | `sheets:read` |    ✅    | tabs + header row + row count (schema discovery) |
| `sheets_query_rows`           | `sheets:read` |    ✅    | filter rows ตาม column — **tool หลัก**           |
| `sheets_read_range`           | `sheets:read` |    ✅    | อ่าน A1 range ตรงๆ (escape hatch)                |
| `drive_list_folder`           | `drive:read`  |    ✅    | list ไฟล์ใน folder ที่ allowlist                 |
| `drive_get_file`              | `drive:read`  |    ✅    | metadata + short-lived link                      |

### 4.1 `sheets_describe_spreadsheet` — ตัวที่ขาดไม่ได้

Sheets **ไม่มี schema** — agent ไม่มีทางรู้ว่า tab ชื่ออะไร column ไหนคืออะไร ถ้าไม่มี tool นี้ agent จะเดา range แล้วพังทุกครั้ง เรียก tool นี้ก่อนเสมอ

```jsonc
// input
{
  "spreadsheet_id": "string, required",
  "include_sample_rows": "int, 0-5, default 0"  // ตัวอย่างข้อมูลช่วยให้ agent เข้าใจ format
}

// output
{
  "spreadsheet_id": "1AbC...",
  "title": "ระบบสัญญา 2568",
  "sheets": [
    {
      "name": "Contracts",
      "row_count": 12450,
      "columns": [
        {"index": 0, "letter": "A", "header": "contract_id"},
        {"index": 1, "letter": "B", "header": "partner_name"}
      ],
      "sample_rows": []
    }
  ]
}
```

### 4.2 `sheets_query_rows` — workhorse

```jsonc
// input
{
  "spreadsheet_id": "string, required",
  "sheet_name": "string, required",
  "filters": [                                  // AND กันทุกตัว
    {"column": "partner_name", "op": "eq", "value": "หจก. ก่อสร้าง"},
    {"column": "status", "op": "in", "value": ["draft", "pending"]}
  ],
  "columns": ["contract_id", "status"],         // projection — ลด context ที่ส่งกลับ
  "limit": "int, 1-200, default 50",
  "offset": "int, default 0",
  "response_format": "markdown | json, default markdown"
}

// output
{
  "total": 340, "count": 50, "offset": 0,
  "has_more": true, "next_offset": 50,
  "rows": [{"contract_id": "C-0012", "status": "draft"}]
}
```

**Operators ที่รองรับ:** `eq`, `neq`, `contains`, `gt`, `lt`, `gte`, `lte`, `in`
**ไม่รองรับ:** free-form expression, formula, raw query string — จงใจ (ดู §6)

### 4.3 `drive_get_file`

คืน **signed short-lived URL (TTL ≤ 15 นาที)** ไม่ใช่ permanent link — ป้องกัน link รั่วจาก conversation log ของ agent

---

## 5. RBAC mapping

ใช้ semantics เดิม (`*` > `resource:verb` > `resource:*`) ไม่ต้องแก้ matcher

| Permission                    | ให้อะไร                                     |
| ----------------------------- | ------------------------------------------- |
| `sheets:read`                 | ทุก `sheets_*` read tool                    |
| `drive:read`                  | ทุก `drive_*` read tool                     |
| `sheets:write`                | (phase 2)                                   |
| `connector:read/write/delete` | CRUD ตัว connector เอง — ของเดิม ไม่เปลี่ยน |

### ⚠️ จุดที่ middleware เดิมใช้ไม่ได้ตรงๆ

`RequirePermission(action)` เป็น Echo middleware ผูกกับ route — แต่ MCP tool call **ทุกตัวเข้า route เดียวกัน** (`POST /mcp/:connectorId`) permission ต่างกันตาม tool ใน body

ต้องแยก permission matcher ออกมาเป็นฟังก์ชันที่เรียกได้จากใน handler:

```go
// internal/module/rbac — ดึง logic ที่ middleware ใช้อยู่ออกมา reuse
func (s *Service) HasPermission(ctx, userID, orgID uuid.UUID, action string) (bool, error)
```

แล้ว MCP handler เรียกเองต่อ tool — middleware เดิมยังเรียกฟังก์ชันเดียวกันนี้ ไม่ duplicate logic

**และ `tools/list` ต้อง filter ตาม permission ของ caller** — agent ไม่ควรเห็น tool ที่เรียกไม่ได้ (ลด context เปล่า + ไม่ leak ว่ามี capability อะไรอยู่)

---

## 6. Guardrails

### Query sanitization

Sheets ไม่มี SQL injection แต่มี 2 surface จริง:

1. **Formula injection** — ค่าที่ขึ้นต้นด้วย `=`, `+`, `-`, `@` ที่ agent ส่งมาใน filter ต้อง treat เป็น literal string เสมอ ห้ามส่งเข้า Google Query Language
2. **Range traversal** — `spreadsheet_id` / `sheet_name` ต้อง validate กับ allowlist ใน config **ทุกครั้ง** ไม่ใช่แค่ตอนสร้าง connector

**ตัดสินใจ: ไม่ expose Google Visualization Query Language ให้ agent เขียนเอง** — filter เป็น structured DSL (§4.2) เท่านั้น แลก flexibility กับ attack surface ที่ควบคุมได้

### Rate limiting

Google Sheets API quota: ~300 req/min ต่อ project, ~60 req/min ต่อ user → agent ที่ loop เรียกจะเผา quota ของทั้ง org

- Token bucket ใน Redis: `mcp:ratelimit:<connectorId>` — เสนอ 60 req/min ต่อ connector, configurable
- ทับ key convention เดิมได้เลย ไม่ต้องเพิ่ม infra

### Result caps

- `limit` max 200, default 50
- Response body cap (เสนอ ~256KB) — ถ้าเกิน truncate แล้วบอก agent ให้ใช้ `columns` projection หรือแคบ filter ลง
- **ห้ามโหลดทั้ง sheet เข้า memory** — 87GB ทำให้ข้อนี้เป็นเรื่องคอขาดบาดตาย

---

## 7. Audit events

ต่อยอด audit log เดิม (best-effort, ไม่ fail request ตาม ground rule)

| Action                | Metadata                                                                                               |
| --------------------- | ------------------------------------------------------------------------------------------------------ |
| `mcp.session.started` | `connector_id`, `client_name`, `client_version`                                                        |
| `mcp.tool.called`     | `connector_id`, `tool`, `spreadsheet_id`, `sheet_name`, `filter_columns[]`, `row_count`, `duration_ms` |
| `mcp.tool.denied`     | `connector_id`, `tool`, `missing_permission`                                                           |
| `mcp.ratelimit.hit`   | `connector_id`, `tool`                                                                                 |

**สำคัญ: log ชื่อ column ที่ filter ได้ แต่ห้าม log `value` ที่ filter** — ค่าพวกนั้นคือข้อมูลธุรกิจจริง (ชื่อ partner, เลขสัญญา) ตรงกับ ground rule "log individual fields, never a whole struct"

---

## 8. Error codes

คืนใน result object (`isError: true`) ไม่ใช่ protocol-level error ตาม MCP spec — และต้อง**บอกทางแก้** ให้ agent

| Code                      | ข้อความแนะนำ                                                                        |
| ------------------------- | ----------------------------------------------------------------------------------- |
| `SPREADSHEET_NOT_ALLOWED` | ไม่อยู่ใน allowlist — เรียก `sheets_list_spreadsheets` เพื่อดูว่าเข้าถึงอะไรได้บ้าง |
| `SHEET_NOT_FOUND`         | tab ไม่มีอยู่ — เรียก `sheets_describe_spreadsheet` ก่อน                            |
| `COLUMN_NOT_FOUND`        | column ไม่มี — ดู header จาก `sheets_describe_spreadsheet`                          |
| `RESULT_TOO_LARGE`        | แนะนำให้ใส่ `columns` projection หรือลด `limit`                                     |
| `RATE_LIMITED`            | บอก retry-after เป็นวินาที                                                          |
| `UPSTREAM_AUTH_FAILED`    | OAuth refresh token หมดอายุ — ต้องให้ owner re-auth ที่ dashboard                   |

---

## 9. Phase 2 (ยังไม่ build — จดไว้กันลืม)

Write tools ต้องคิดเรื่อง safety เพิ่มก่อน (confirmation flow, dry-run, rollback):

- `sheets_append_row` — `destructiveHint: false`, `idempotentHint: false`
- `sheets_update_cells` — `destructiveHint: true` ⚠️
- `drive_upload_file`
- LINE adapter (ส่งข้อความไป partner) — **action-type tool, risk สูงกว่า read มาก** ควรทำหลัง read stable

---

## 10. คำถามที่ยังต้องตัดสินใจ

1. **MCP client auth** — เอา PAT (option A) ไหม? กระทบ migration + module ใหม่
2. **OAuth onboarding flow** — ลูกค้าจะ authorize Google ยังไง? ต้องมีหน้า OAuth consent ใน Next.js dashboard (งานที่ยังไม่อยู่ใน roadmap เดิม)
3. **Header row detection** — สมมติแถวแรกเป็น header เสมอ หรือให้ config ระบุ? ระบบจริงของลูกค้าอาจมี title row ข้างบน
4. **Multi-tab join** — agent จะถามข้าม tab (สัญญา ↔ partner) แน่นอน จะให้ agent เรียกหลาย tool แล้ว join เอง หรือทำ workflow tool ให้? (MCP best practice บอกว่าเริ่มจาก coverage ก่อน workflow — **เสนอให้ agent join เอง** ใน MVP)
