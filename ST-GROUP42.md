# 軟體測試期末專題 — Group 42

2026 軟體測試課程期末專題（Category 4: Testing Framework or Tool Development）的
工作紀錄。被測系統是這個 repo 本身。

本檔案只記錄**實際量測或從程式碼查證過**的事實。沒跑過的數字不寫。

## 基準 commit

```
2c88557  2026-11-08  Merge pull request #120 from NYCU-SDC/feat/overlap-forbid
```

這是上游 `NYCU-SDC/caravanserai` 的 `main` 在 2026-11-10 的 HEAD，本專題所有量測都在
它上面取得。之後釘住不動：上游在 2026-11-14 已前進到 `88b3482`，但基準不隨之移動，
否則先前的量測全部失效。

釘在 `2c88557` 而非更早的版本，有兩個獨立理由：

1. 全部的環境量測都是在這個版本上取得的。
2. `spec.overlap = Forbid` 只存在於這個版本（PR #120）。釋放授權的等價分割表需要
   `Forbid` 的拒絕案例，更早的基準沒有這條路徑可測。

## 分支規約

課程的工作一律開在 `st/...`，例如 `st/timing-injection`。

不要開在 `feat/*`、`fix/*`、`refactor/*`、`chore/*`——那些是維護者對上游開 PR 用的。

在網頁上開 PR 時，GitHub 預設的 base repository 是上游。要手動改成這個 fork。

## 已完成

### HTTP 故障注入代理（`test/injection/httpfault`）

可同步控制的反向代理，放在 cara-server 前面，讓測試決定 agent 的哪一次呼叫、在哪一刻失敗。

它不需要改動任何產品程式碼：agent 只透過 `SERVER_URL` 找到 server，所以把那個環境變數
指向代理就行，cara 以出貨的樣子執行。

重點不是它能回傳錯誤——把 server 關掉也能——而是測試能決定錯誤**何時**落下。規則帶
`hold` 時，相符的請求會被凍結在代理裡並通報；測試這時才去建立情境需要的狀態，然後才
放行成失敗。系統與測試都停在已知的點上，時序才是可重現的，而不是靠運氣。

每個決定都記在帶時間戳的有序事件日誌裡，所以測試可以斷言發生順序，也能把**注入的失敗**
與**呼叫端自己放棄**分開。後者會發生：agent 的 HTTP client 自己有逾時（本基準為 10 秒，
`internal/agent/client.go:91`），凍結超過它，請求會在代理還沒回應前就失敗。那是另一種
事件，日誌會說清楚——否則測試會把從未送達的故障報成通過。

執行：

```bash
make test-faultproxy-smoke
```

這支用幾行 Python 代替 cara-server，不需要 Docker、不需要資料庫，約 2.6 秒跑完，
便宜到可以每次 push 都跑。Go 測試另外涵蓋代理本身的行為：

```bash
go test -race -count=20 ./test/injection/...
```

### 變異測試 PoC：既有測試對一條安全規則的盲區（2026-11-26）

目的：在投入框架之前，先證明「既有測試漏掉、而我們抓得到」的缺陷真的存在。方法是變異測試——
故意植入一個小缺陷，看哪一層測試會發現。

**cara 本身沒有這個缺陷。** 量的是測試套件的敏感度：如果有人改掉這行保護，現有測試會不會發現。

受測規則：所有權未知時不得開始清理。`internal/agent/loop.go:381-390` 在
`ListProjectsAssignedToNode` 失敗時做兩件事——重算刪除寬限期（`resetGrace()`），然後
`return` 放棄這一輪。

| 變異體 | 內容 | 補丁 |
|---|---|---|
| M1 | 刪掉 `return`：把「問不到」當成「什麼都不擁有」 | `test/injection/mutations/m1-error-treated-as-empty.patch` |
| M2 | 刪掉 `resetGrace()`：失聯後不重算寬限期 | `test/injection/mutations/m2-missing-reset-grace.patch` |

結果（開發機，基準 `2c88557` + `51c8b15`）：

| 測試層 | 規模 | 耗時 | M1 | M2 |
|---|---|---|---|---|
| `make test` | 28 個套件 | 8 秒 | 漏抓 | **抓到** |
| `make test-integration` | 319 個測試，0 跳過 | 49 秒 | 漏抓 | 未跑 |
| `scripts/e2e/orphan-cleanup.sh` | 326 行 | 230 秒 | 漏抓（腳本印出 PASS） | 未跑 |
| `internal/agent/poc_gap_test.go` | 1 個斷言測試 | 0.009 秒 | **抓到** | — |

M2 是被既有的 `TestReconcileProjects_ServerErrorResetsDeletionGrace` 抓到的。所以結論不是
「cara 沒測這條規則」，而是：**這條保護有兩半，測試套件守住了「寬限期要重算」，沒有守住
「根本不准動手」。**

M1 是真的安全違規，不是無害的改動：套用 M1 後 `poc_gap_test.go` 失敗並回報
`Should be empty, but was [default/poc-target]`——agent 在無法確認所有權時停掉了容器。
乾淨的 cara 上同一支測試通過，所以它不會誤報。

`orphan-cleanup.sh` 漏抓的原因：它用 SQL 直接改 `nodeRef` 來製造所有權轉移，agent 的查詢
隨後**成功**並回答「不是你的了」。整支腳本只走查詢成功的路徑，查詢失敗那條路在真實多行程
環境中從未被執行過。

範圍限制：

- 一個存活變異體只是一個資料點。
- 只跑了 `orphan-cleanup.sh`。它是唯一會執行到這段程式碼的端對端腳本；其他四支未跑。
- `poc_gap_test.go` 必須放在 `internal/agent/`，因為 `reconcileProjects` 未匯出。它是測試檔，
  不改變產品行為。

重現（補丁一律套上→跑→還原，提交的產品碼不變）：

```bash
git apply test/injection/mutations/m1-error-treated-as-empty.patch
make test                    # 預期通過 = 漏抓
make test-integration        # 預期通過 = 漏抓（需要 Docker）
./scripts/e2e/orphan-cleanup.sh   # 預期通過 = 漏抓（需要 Docker，約 4 分鐘）
go test -run TestPoC_OwnershipUnknownMustNotTouchContainers ./internal/agent/   # 預期失敗 = 抓到
git checkout -- internal/agent/loop.go
```

### 時序變異體：現有測試已經殺掉大部分（2026-11-26）

問題：有沒有「只有控制時機才殺得掉」的變異體？這是時序框架存在的理由，所以先驗證。

做法：針對期限、倒數、重試次數做 12 個變異體（多數是把 `>=` 改成 `>` 這類邊界差一點的
改法），加上先前的 M1、M2，用 `test/injection/mutations/run.sh --integration` 批次跑。
全部 14 個跑完 3 分 27 秒，產品碼每次都還原。

| 變異體 | 改法 | 單元 | 整合 |
|---|---|---|---|
| a1 orphan 刪除寬限期 | `>=` → `>` | 殺掉 | — |
| a2 復原驗證期限 | `>=` → `>` | 殺掉 | — |
| a3 復原退避 | `<` → `<=` | 殺掉 | — |
| a4 退避從第一次嘗試起算 | 刪掉更新 `lastAttemptAt` | 殺掉 | — |
| a5 容器重啟觀察期限 | `>=` → `>` | 殺掉 | — |
| a6 恢復後不清除嘗試次數 | 刪掉 `delete(t.entries, key)` | 殺掉 | — |
| m1 查詢失敗當成空清單 | 刪掉 `return` | 殺掉（被 `poc_gap_test.go`） | — |
| m2 不重算寬限期 | 刪掉 `resetGrace()` | 殺掉 | — |
| s1 沿用上一次事件的計時 | `incidentClock` 不比對心跳 | 殺掉 | — |
| s2 Running 寬限期 | `<` → `<=` | **存活** | **存活** |
| s3 終止逾時 | `<` → `<=` | 殺掉 | — |
| s4 心跳逾時判定 | `>` → `>=` | **存活** | **存活** |
| s5 心跳恢復判定 | `<=` → `<` | **存活** | **存活** |
| s6 NotReadyAt 不重蓋 | `stampedSince` 恆真 | 存活 | 殺掉 |

結論：

- **12 個時序變異體，現有測試殺掉 9 個。** cara 的單元測試大量使用假時鐘，agent 端六個全滅。
- 存活的 s2、s4、s5 全是「剛好等於期限的那一瞬間」的差別。用普通的假時鐘單元測試就能殺掉：
  把心跳年齡設成剛好 `NodeHeartbeatTimeout`，乾淨的 cara 通過、套用 s4 失敗（臨時測試，已刪除）。
- 反過來，真實多行程環境**殺不掉**它們：node 健康檢查每 30 秒才掃一次
  （`nodeSeedInterval`），T 與 T+1 奈秒落在同一次掃描裡，觀察不到差別。
- 所以這一類變異體不是時序框架的證據。這次沒有找到「只有控制時機才殺得掉」的變異體。

還沒試的方向：跨行程的事件交錯（例如兩個寫入的先後、查詢在途中狀態被改）。本次 12 個都是
單一行程內的期限判斷，沒有涵蓋。

### 交錯變異體：找到兩個真缺口，但不是「只有控制順序才殺得掉」（2026-11-26）

問題：前一批都是單一行程內的期限判斷。這批改看「兩個寫入撞在一起」的保護——
`internal/store/postgres/store.go` 的 compare-and-swap（用 `resource_version` 擋掉讀與寫之間
被別人改過的寫入）、UID 與指派代數的柵欄、`FOR UPDATE` 列鎖。

| 變異體 | 改法 | 單元 | 整合 |
|---|---|---|---|
| i1 一般 CAS 不檢查版本 | 拿掉 `resource_version = $7`（`swapProjectStatus`） | 存活 | 殺掉 |
| i2 agent 回報的 fenced swap 不檢查版本 | 同上，`swapProjectStatusFenced` | 存活 | **存活** |
| i3 UID 釘住的 swap 不檢查版本 | 同上，兩處 | 存活 | **存活** |
| i4 UID 釘住的 swap 不檢查 UID | 拿掉 `uid = $8`，兩處 | 存活 | 殺掉 |
| i5 柵欄不檢查指派代數 | 拿掉 `assignmentGeneration` 條件 | 存活 | 殺掉 |
| i6 finalize 不鎖列 | `FOR UPDATE` → 空字串，兩處 | 存活 | 殺掉 |

i2、i3 是真的缺陷：讀完之後、寫入之前，如果另一個寫入先落地，它會被整個覆蓋掉（lost update）。
用 `internal/store/postgres/interleave_probe_e2e_test.go` 證明：

| 探針 | 乾淨的 cara | i2 | i3 |
|---|---|---|---|
| 控制順序：用既有的 `beforeSwap` 鉤子把一個寫入放在讀與寫之間 | 通過 | 通過（鉤子不在這條路徑上） | **失敗** |
| 不控制順序：兩個寫入同時出發，重複 300 次 | 0/300 遺失 | **184/300 遺失** | **192/300 遺失** |

結論：

- **這是現有 319 個整合測試沒有守住的缺口**，對應提案的「並行更新安全（OCC）」檢查器。既有的
  OCC 測試（`project_status_occ_test.go`）用假的 CAS 測重試策略，沒有對真的 SQL 製造並行寫入。
- 但它們**不需要**控制順序才殺得掉：本機上兩個寫入同時出發，約六成會撞到。控制順序的好處是
  確定性（一次就中、2 秒），不是唯一性。
- 真實系統裡兩個寫入撞在一起的機率遠低於本機壓測（agent 走網路、controller 每 30 秒才掃一次），
  所以這類缺陷在端對端環境中幾乎不會自然出現。

### 當機變異體：agent 當掉等於「上一步失敗、下次重試」（2026-11-26）

問題：在兩個步驟之間讓 agent 當掉再重啟，會不會出現假環境測不到的缺陷？

找重啟後必須接著做完的流程。agent 端有兩個：清理（刪資源 → 回報 ResourcesRemoved）與
資料隔離（搬走 Managed 資料 → 回報）。兩個都可能在「做完、還沒回報」時當掉。

| 變異體 | 改法 | 單元 | 整合 |
|---|---|---|---|
| c1 已經清乾淨就不回報 | `runCleanupTask` 開頭：沒有剩餘資源就 `return` | **存活** | **存活** |
| c2 資料隔離不能重做 | `restore.Quarantine` 找不到資料夾時回錯誤 | 殺掉 | — |

c1 是真的缺陷：資源刪了、回報沒送出去，之後每一輪都看到「沒東西可刪」就離開，義務永遠
不會解除，刪除永遠完成不了。

但它**不需要當機**就能觸發：第一次回報失敗（server 暫時連不上）就會進到同樣的狀態。
`internal/agent/poc_cleanup_retry_test.go` 用假 server 讓第一次回報失敗、第二輪再看——乾淨的
cara 通過，套用 c1 失敗。

原因是 cara 的設計：agent 不在記憶體裡保留任何清理相關的狀態，清理任務每一輪都從 server
重新列出來，WorkloadStopped 也是從 server 讀。所以「在兩步之間當掉再重啟」對 agent 來說就等於
「上一步失敗，下一輪重試」，而後者在假環境裡就能測。

另外，端對端腳本其實已經會重啟 agent：`spec-update.sh` 停掉再啟動 agent，
`managed-failover.sh` 讓 node-a 的 agent 回來後檢查它沒有重開不屬於它的工作負載。

### 目前的總表

| | 數量 |
|---|---|
| 變異體總數 | 22 |
| 現有測試殺掉 | 15（68%） |
| 存活 | 7：m1、i2、i3、c1（真缺陷），s2、s4、s5（剛好等於期限的瞬間） |
| 加上本專題的測試後殺掉 | 19（86%） |

本專題補上的四個缺口有同一個形狀：**一個操作失敗或被別人搶先之後，接下來的行為沒有被測。**

| 缺口 | 注入的故障 | 補上的測試 |
|---|---|---|
| m1 | 所有權查詢失敗 | `internal/agent/poc_gap_test.go` |
| c1 | 回報 ResourcesRemoved 失敗 | `internal/agent/poc_cleanup_retry_test.go` |
| i2 | agent 回報時另一個寫入先落地 | `internal/store/postgres/interleave_probe_e2e_test.go` |
| i3 | 指定 UID 的寫入時另一個寫入先落地 | 同上 |

22 個變異體裡，沒有一個是「只有控制時機、順序或讓行程當掉才殺得掉」的。

## 已驗證的環境事實

### 既有 E2E 腳本

行數在 `2c88557` 上統計。耗時是**單次**量測，兩種環境分開列——同一支腳本在兩種機器上
差兩成以上，混在一起看會以為自相矛盾。

| 腳本 | 行數 | 開發機（20 核） | GitHub runner（4 核） |
|---|---|---|---|
| `local-recovery.sh` | 748 | 490 秒 | 588 秒 |
| `managed-failover.sh` | 679 | 495 秒 | **失敗**（見下） |
| `postgres-differential.sh` | 513 | 139 秒 | 未量測 |
| `spec-update.sh` | 370 | 未量測 | 未量測 |
| `orphan-cleanup.sh` | 326 | 228 秒 | 318 秒 |

runner 的數字來自 fork 的 workflow run
[41990653064](https://github.com/Hankkdd/caravanserai/actions/runs/41990653064)（2026-11-05，
跑的是當時的樹，不是 `2c88557`）。Docker-in-Docker 在 GitHub-hosted runner 上可行，
這次 run 證實了這點。

### `managed-failover.sh` 帶著一個沒寫下來的環境假設

它在 4 核的 runner 上**必然**失敗：

```
project: failover-stateless, size: Medium
rejected: node "node-a" lacks cpu: 3000m in use + 1000m requested > 3500m allocatable
```

`allocatable = 核心數 × 1000 − AGENT_RESERVED_CPU`（`internal/agent/capacity.go`），
預設保留 500m，所以 4 核只有 3500m，排不下它要的 4000m。開發機（20 核 → 19500m）永遠
看不到這個限制。

把全部四個 project 改成 `"size":"Small"` 之後，在同樣 3500m 的條件下整套通過（495 秒，
含 `NoRestoreSource`、`LegacyProvenance`、checksum、quarantine）。所以資源是腳本的缺陷，
時間才是真正的限制。

這類「對執行機器的隱含假設」正是本框架要攤開的東西之一。

### Server 端的期限，哪些已經可注入

查 `2c88557` 的結果——這會改變工作量估計：

| Server 期限 | 有 option？ | 位置 |
|---|---|---|
| 健康檢查週期 `seedInterval` | **有** | `internal/server/controller/node_health.go:59,68-77` |
| `runningGracePeriod`（3 分鐘） | **有** | `internal/server/controller/project_rescheduler.go:170-172` |
| `terminatingTimeout` | **有** | `internal/server/controller/project_rescheduler.go:166-168` |
| `NodeHeartbeatTimeout`（90 秒） | **沒有**，是 plain const | `internal/server/controller/node_health.go:19` |

而 `cmd/cara-server/main.go:228` 一個 option 都沒傳。所以「為 Server 補上健康檢查週期
設定」其實是把既有的 option 接到設定檔，不是實作新機制；真正需要新增 option 的只有
失聯門檻一個。

### Agent 端的期限，哪些還不能從外部控制

| Agent 期限／參數 | 外部可控？ | 位置 |
|---|---|---|
| `HEARTBEAT_INTERVAL` | **可**（環境變數） | `internal/config/agentconfig.go:37` |
| 輪詢週期 `defaultPollInterval`（10 秒） | 僅行程內 option | `internal/agent/loop.go:151,161` |
| `recoveryVerifyTimeout`（30 秒） | 否 | `internal/agent/recovery.go:23` |
| `transientObservationTimeout`（30 秒） | 否 | `internal/agent/recovery.go:31` |
| `maxRecoveryAttempts`（3 次） | 否 | `internal/agent/recovery.go:15` |
| `orphanGracePeriod`（3 分鐘） | 否，`internal/config/agentconfig.go` 查無此欄位 | `internal/agent/orphan.go:23` |
| HTTP client 逾時（10 秒） | 否 | `internal/agent/client.go:91` |

`recoveryVerifyTimeout` 與 `transientObservationTimeout` 同為 30 秒。邊界測試寫
「復原驗證期限 30 秒」時要指明是哪一個，否則檢查器會驗錯對象。

HTTP client 逾時（10 秒）與輪詢週期（10 秒）同值。代理凍結一個請求的預算就是前者，
兩個 10 秒撞在一起很容易看錯。

### Agent 的兩個 project 查詢共用同一條路徑

```
GET /api/v1/projects?phase=Scheduled&nodeRef=node-a   internal/agent/client.go:309  該啟動什麼
GET /api/v1/projects?nodeRef=node-a                   internal/agent/client.go:343  還擁有什麼
```

只有第二個會驅動破壞性清理（`internal/agent/loop.go:380`）。注入規則必須要求 `nodeRef`
並**排除** `phase=`，否則會注入錯的那一次呼叫，而且看起來一樣像成功。
`test/injection/httpfault` 的測試與 smoke test 都明確守住這條區別。

## 不要做的事

- 不要推任何東西到 `NYCU-SDC/caravanserai`，也不要開 PR 到那裡。
- 不要改變 cara 的產品行為。新增測試用的注入介面與設定欄位可以，但必須保留原有預設值，
  正常執行時的行為不變。測試中發現的缺陷另外記錄，由維護者決定是否修。
- 不要把沒有實測過的數字寫進提案或報告。
- 不要把 API key 或 token 寫進 repo。Claude API key 放
  Settings → Secrets and variables → Actions。
