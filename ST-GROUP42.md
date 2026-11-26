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
