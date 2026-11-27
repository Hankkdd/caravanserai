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

| | 手寫 | Codex 提出 | 合計 |
|---|---|---|---|
| 變異體 | 29 | 8 | 37 |
| 原本的測試殺掉 | 21（72%） | 0 | 21（57%） |
| 原本的測試漏抓 | 8：m1、i2、i3、c1（真缺陷），s2、s4、s5（剛好等於期限的瞬間），v6（等價） | 8（看起來都是真缺陷） | 16 |
| 加上本專題的測試後殺掉 | 25（86%） | 1 | 26（70%） |
| 再加上 Codex 補寫的 8 支測試 | 25（86%） | 8 | 33（89%） |

最後剩下的 4 個：s2、s4、s5（剛好等於期限的那一瞬間）與 v6（等價變異體）。扣掉等價變異體是 33/36。

手寫的四個缺口有同一個形狀：**一個操作失敗或被別人搶先之後，接下來的行為沒有被測。**

| 缺口 | 注入的故障 | 補上的測試 |
|---|---|---|
| m1 | 所有權查詢失敗 | `internal/agent/poc_gap_test.go` |
| c1 | 回報 ResourcesRemoved 失敗 | `internal/agent/poc_cleanup_retry_test.go` |
| i2 | agent 回報時另一個寫入先落地 | `internal/store/postgres/interleave_probe_e2e_test.go` |
| i3 | 指定 UID 的寫入時另一個寫入先落地 | 同上 |

37 個變異體裡，沒有一個是「只有控制時機、順序或讓行程當掉才殺得掉」的。

### AI agent PoC：Codex 依變異體寫測試，4 個全部殺掉（2026-11-26）

問題：AI agent 能不能自己寫出抓到缺陷的測試？判定交給確定性的流程，AI 不參與評分。

做法：

- 受試者：Agent Hub 上的 Codex（`gpt-6.1-sol`，effort high），每個變異體一個全新的任務，
  彼此獨立，也看不到這份紀錄或本專題寫的測試。Hub 上的 Claude 當時沒有登入，所以只用 Codex。
- 每個任務拿到一份乾淨的 cara（基準 `2c88557`，不含本專題的任何檔案）加上一個變異體補丁，
  要求新增一個 `_ai_test.go`：乾淨時通過、套用補丁後失敗，不准改既有檔案、不准讀原始碼。
  完整 prompt 在 `test/injection/ai-poc/PROMPT.txt`。
- 判定不採信 AI 的自我回報：只把它新增的那一個檔案複製到另一份全新的乾淨副本，乾淨的 cara
  連跑 5 次都要通過，套用變異體要失敗；另外比對它的資料夾，確認只新增一個檔案、沒改既有檔案。
  判定腳本存在 `test/injection/ai-poc/judge.sh`（用 `AGENT_ROOT` 指向 agent 工作的資料夾）。

結果：

| 缺陷 | 判定 | 本專題手寫的測試 | Codex 寫的測試 |
|---|---|---|---|
| m1 | ✓ 殺掉 | 一種情況：server 回 500，不准停任何容器 | 三種情況：503、回傳格式壞掉、**加上對照組**（成功拿到空清單時必須停）；另外檢查路由沒被移除 |
| c1 | ✓ 殺掉 | 讓第一次回報失敗，看第二輪有沒有補報 | 直接設定「資源已經不在」，看有沒有回報；更直接 |
| i2 | ✓ 殺掉 | 兩個寫入同時送出 300 次，184 次撞到（**靠機率**） | 在 mutate callback 裡同步寫入另一筆，**確定性**，一次就中 |
| i3 | ✓ 殺掉 | 用既有的 `beforeSwap` 鉤子（確定性） | 同 i2 的 callback 手法，不需要鉤子 |

4 個任務都只新增一個檔案、沒有改既有檔案、產品碼都還原了。Hub task id 在
`test/injection/ai-poc/tasks.tsv`，四支測試的副本在 `test/injection/ai-poc/tests/`。

值得注意的是 i2：手寫時找不到確定性的切入點（fenced 路徑沒有 `beforeSwap` 鉤子），只好用壓力測試；
Codex 發現 mutate callback 本身就在「讀」與「寫」之間執行，在那裡寫入就是確定的順序控制。

缺點：

- i2、i3 自己加了沒被要求的環境變數（名稱還不一致：`CARAVANSERAI_TEST_DATABASE_URL`、
  `CARA_TEST_DATABASE_URL`）。
- 斷言 `ResourceVersion + 2` 綁得很緊，cara 的實作細節一改就會誤報。
- 在 mutate callback 裡寫資料庫，依賴「callback 在讀與寫之間執行」這個實作細節。

範圍限制：

- 只有 4 個樣本、1 個模型、每個只跑 1 次。重跑是否穩定還沒量。
- AI 拿到的是**已知的變異體補丁**，等於已經被告知錯在哪一行。這次量的是「依已知缺陷寫測試」，
  不是「自己找出盲區」。後者見下面兩節。

### 過期資料變異體（參考 Sieve 的 stale view）：cara 守得很好（2026-11-26）

問題：Sieve（OSDI 2022）對叢集控制器注入「讀到過期狀態」找到很多錯誤。agent 依輪詢到的舊資料
動手，會不會出事？

先查程式碼：會動到 Docker 的路徑（本地復原、更新規格）動手前都會**重讀一次並檢查柵欄**
（`checkRecoveryFence`：UID、指派代數、nodeRef、Running），`loop.go` 的註解直接寫出這是為了
防止依過期資料重啟已經被轉走的工作負載。

唯一沒有重讀的是啟動 Scheduled project 的路徑（`reconcileOne`）。一度以為是缺陷，查完**不是**：
Scheduled project 在節點失聯時要被轉走，也得通過同一道釋放關卡（`adapter/project.go`）。
預設設定下不會轉走；只有開啟 grace-expired release 且允許重疊時才會轉，而那種設定本來就接受重疊。

| 變異體 | 改法 | 單元 | 整合 |
|---|---|---|---|
| v1 柵欄不比指派代數 | 刪掉 `checkRecoveryFence` 的 generation 條件 | 殺掉 | — |
| v2 柵欄不比 nodeRef | 刪掉 nodeRef 條件 | 殺掉 | — |
| v3 柵欄不比 phase | 刪掉 Running 條件 | 殺掉 | — |
| v4 柵欄不比 UID | 刪掉 UID 條件 | 殺掉 | — |
| v5 復原看舊資料判斷維護中 | `current` → `p` | 殺掉 | — |
| v6 更新規格看舊資料判斷維護中 | `current` → `p` | 存活 | 存活 |
| v7 更新規格看舊資料判斷 WorkloadStopped | `current` → `p` | 殺掉 | — |

v6 是**等價變異體**：Maintenance 只由 agent 自己在備份時設定，而備份與更新規格都要先拿到 agent
內部協調器的同一把鎖（`backup/runner.go` 的 `TryClaim`）。`backupsupport.go` 的註解也寫明
Maintenance「是給人看的，不是鎖」。所以新舊資料的判斷結果不會不同。

結論：過期資料這類，現有測試 7 個殺掉 6 個，剩下 1 個等價。cara 對 stale view 的防護與測試都很完整。

### AI agent 自己提出變異體：8 個全部是現有測試的盲區（2026-11-26）

問題：前一個 PoC 是把變異體（等於錯在哪一行）直接給 AI。這次參考 Meta 的 ACH 前半段：
只給 AI 安全規則和乾淨的程式碼，讓它自己找出測試套件的盲區。

做法：

- Codex（`gpt-6.1-sol`，effort high）一個任務，拿到乾淨的 cara（基準 `2c88557`）與提案的安全規則
  （R1–R9），要求寫 8 個「開發者可能真的會犯、違反某條規則、現有測試抓不到」的變異體，存成補丁。
  它可以自己跑單元測試挑存活的；它的環境沒有 Docker，整合測試沒跑。
  prompt 在 `test/injection/ai-poc/mutant-proposal/PROMPT.txt`，Hub task id 在 `tasks.tsv`。
- 判定由 `run.sh --integration` 做，跑兩個基準：只有 cara 原本的測試（另開一個乾淨的上游 worktree），
  以及加上本專題補的測試。補丁存在 `test/injection/mutations/ai-*.patch`。

| 變異體 | 規則 | 內容 | 原本的測試 | 加上本專題的測試 |
|---|---|---|---|---|
| ai-grace-config | R1 | `main.go` 不讀設定，grace-expired release 永遠開啟（預設是關） | 存活 | 存活 |
| ai-termination-config | R5 | `main.go` 把 Running 寬限期（預設 3 分鐘）接成刪除逾時（預設 10 分鐘） | 存活 | 存活 |
| ai-cleanup-last-kind | R9 | 檢查剩餘資源時只保留最後一種；最後一種是永遠回報「沒有剩餘」的代理路由，所以**容器還在也會回報清乾淨** | 存活 | 存活 |
| ai-apply-clears-stop | R3 | server 更新規格時清掉 WorkloadStopped 紀錄 | 存活 | 存活 |
| ai-snapshot-decode | R8 | 所有權清單解析失敗時當成空清單 | 存活 | 存活 |
| ai-fenced-cas | R2 | agent 回報狀態時用沒有柵欄的 CAS | 存活 | 存活 |
| ai-fenced-conflict | R7 | 版本衝突直接回報成功，寫入被丟掉 | 存活 | **殺掉**（交錯探針） |
| ai-preflight-budget | R4 | 復原前置檢查失敗時也扣掉一次嘗試 | 存活 | 存活 |

8 個逐一讀過，都會改變行為，看不出等價變異體。原本的單元測試與 319 個整合測試**一個都沒抓到**；
整合測試是我們補跑的，所以這不是 Codex 自己挑選的結果。

跟手寫變異體比較：

| | 手寫（29 個，四輪） | Codex 提出（8 個，一個任務約 7 分鐘） |
|---|---|---|
| 改法 | 多數是 `>=` 改 `>`、拿掉一個條件 | 設定接錯、錯誤被吞掉、結果合併錯、狀態被清掉 |
| 原本的測試漏抓 | 8 個，其中真缺陷 4 個 | 8 個，都看起來是真缺陷 |

注意：Codex 被要求挑存活的，所以「存活比例」不能直接比；可以比的是**找到真缺口的數量與成本**。

新的一類：ai-grace-config 與 ai-termination-config 錯在 `cmd/cara-server/main.go` 的接線。
單元與整合測試都直接建構元件，不經過 `main.go`，結構上就測不到；只有跑真的執行檔、檢查實際
行為的測試才可能抓到。這正好是「在真實環境驗證」的用武之地。

另外，ai-snapshot-decode 與 m1 是同一條規則（R8）的不同路徑。本專題的 `poc_gap_test.go` 只測 server
回錯誤，殺不掉它；前一個 PoC 裡 Codex 寫的 m1 測試多測了「回傳格式壞掉」，那一支殺得掉。

### 完整循環：Codex 再替自己找到的 8 個盲區補測試，8 個全部殺掉（2026-11-27）

問題：前一節 AI 找到盲區，這一節讓 AI 把盲區補起來。兩節合起來就是 ACH 的完整循環：
AI 提出變異體 → 確定性流程確認存活 → AI 寫測試 → 確定性流程確認殺掉。

做法與第一個 AI PoC 完全相同（同一份 `PROMPT.txt`、同一個判定腳本、同一個模型與 effort）：
8 個全新的 Codex 任務平行跑，每個拿到乾淨的 cara（基準 `2c88557`）加上一個 `ai-*.patch`，
寫一個 `_ai_test.go`。8 個任務都正常結束，都回報自己驗證過。

判定分兩層，都不採信 AI 的自我回報：

1. **逐支判定**（`judge.sh`）：只把 AI 新增的那一個檔案複製到全新的乾淨副本，乾淨的 cara 連跑 5 次
   都要通過，套用變異體要失敗；比對資料夾確認只新增一個檔案。
2. **整套判定**：8 支測試一起放進乾淨的上游 worktree，先確認乾淨的 cara 單元與整合測試全過，
   再用 `run.sh --integration` 跑 8 個變異體。

| 變異體 | Codex 的測試 | 逐支判定 | 整套判定 |
|---|---|---|---|
| ai-grace-config | `cmd/cara-server/grace_config_ai_test.go` | ✓ | 整合層殺掉 |
| ai-termination-config | `cmd/cara-server/termination_config_ai_test.go` | ✓ | 整合層殺掉 |
| ai-cleanup-last-kind | `internal/agent/cleanup_remaining_ai_test.go` | ✓ | 單元層殺掉 |
| ai-apply-clears-stop | `internal/store/postgres/apply_preserves_stop_ai_test.go` | ✓ | 整合層殺掉 |
| ai-snapshot-decode | `internal/agent/client_snapshot_decode_ai_test.go` | ✓ | 單元層殺掉 |
| ai-fenced-cas | `internal/store/postgres/fenced_status_recreate_ai_test.go` | ✓ | 整合層殺掉 |
| ai-fenced-conflict | `internal/store/postgres/fenced_conflict_ai_test.go` | ✓ | 整合層殺掉 |
| ai-preflight-budget | `internal/agent/preflight_budget_ai_test.go` | ✓ | 單元層殺掉 |

測試副本在 `test/injection/ai-poc/tests/ai-*.go.txt`（存成 `.go.txt`，不會被 `go test` 執行），
Hub task id 在 `tasks.tsv`。

**最值得注意的是兩個 `main.go` 的測試。** 前一節說這類錯誤「單元與整合測試結構上測不到」，
Codex 的解法是讓測試程式在子行程裡重新執行自己、呼叫真正的 `main()`，接一個 Docker 起的
PostgreSQL，事先寫入一個「節點失聯、寬限期已過」（或「刪除逾時已過」）的 project，再觀察真正啟動
的控制器做出什麼決定。這等於 AI 自己寫出了**跑真實入口、在真實資料庫上看行為**的測試，正是
「在真實環境驗證 cara」這個定位要的東西。

為此 `run.sh` 的整合範圍加上了 `./cmd/...`。cara 原本在 `cmd/` 底下沒有 e2e 測試，所以之前的結果不受影響。

缺點（與第一個 AI PoC 相同的老問題）：

- Codex 的環境沒有 Docker，所以三支資料庫測試各自加了「可改用外部資料庫」的環境變數，名稱都不一樣
  （`CARA_TEST_DATABASE_URL`、`CARAVANSERAI_TEST_DATABASE_URL`、`CARA_FENCED_CAS_TEST_DATABASE_URL`）。
- ai-fenced-cas 的測試斷言「刪掉重建後 resource version 相同」，綁住了 cara 的實作細節。
- 在 mutate callback 裡寫資料庫，依賴「callback 在讀與寫之間執行」這個實作細節。
- termination-config 的測試讀了伺服器自己的 log 檔（只在失敗時印出來除錯），判定腳本把它標成
  「可疑」，人工確認不是讀原始碼。

範圍限制：1 個模型、每個變異體只跑 1 次；變異體也是同一個模型提出的，所以「提出」與「補測試」不是
彼此獨立的兩個 agent。重跑穩定性、換模型交叉做都還沒量。

### 單機真實環境情境 PoC：server 半壞時容器要活著（2026-11-27）

問題：前面的變異測試都在單元與整合層。如果把同樣的缺陷編進**真的執行檔**，在真的環境裡製造故障，
情境能不能分辨乾淨的 cara 和有缺陷的 cara？這是往 VM 測試走之前的單機驗證。

為什麼選「server 半壞」：從頭盤點 cara 的測試後，m1、c1、snapshot-decode、cleanup-last-kind、
preflight-budget 五個盲區都是「依賴還活著，但回錯誤或壞資料」。cara 既有的 VM 情境只測網路全斷
（`overlap-partition`），沒有測半壞。

情境：`test/injection/scenarios/server-degraded.sh`。一台機器上跑真的 cara-server、cara-agent、
PostgreSQL（Docker）與工作負載用的 Docker-in-Docker，故障代理放在 agent 與 server 之間，用預設設定。
執行檔一律從 `git archive HEAD` 在暫存目錄建置，`--mutant` 的補丁也套在那裡，工作目錄不會被改。

| 階段 | 故障 | 預期 |
|---|---|---|
| P1 | 所有權查詢回 500，持續 60 秒 | 容器不能被停 |
| P2 | 所有權查詢回 200 但 JSON 被截斷，持續 60 秒 | 容器不能被停 |
| P3 | 真的把 PostgreSQL 停掉 45 秒（低於 90 秒的心跳逾時） | 容器不能被停 |
| P4 對照組 | server 正常回「node-a 什麼都不擁有」 | 容器**必須**被停 |

每個階段用一個新的 project，前一段造成的損害不會影響下一段。P4 用來證明情境看得到「容器被停」；
少了它，容器檢查本身壞掉時 P1–P3 也會通過。故障有沒有真的送到也會檢查：容器活著但故障送到不到 3 次，
判為 INVALID，不算通過。

結果（三個版本在同一台機器上平行跑，乾淨版本另外單獨跑過一次，兩次結果相同）：

| 版本 | P1 回 500 | P2 截斷 JSON | P3 資料庫停掉 | P4 對照組 |
|---|---|---|---|---|
| 乾淨的 cara（`1f7f93f`） | 通過（6 次 500） | 通過（6 次） | 通過（4 次查詢失敗） | 通過（撤掉所有權 8 秒後停） |
| m1（查詢失敗當成空清單） | **失敗**：第一次 500 後 7 秒停掉容器 | **失敗** | **失敗** | 通過 |
| ai-snapshot-decode（解析失敗當成空清單） | 通過 | **失敗**：8 秒停掉容器 | 通過 | 通過 |

這兩個變異體，cara 原本的單元測試和 319 個整合測試都抓不到（見前面幾節）；這個情境不只抓得到，
還能指出是哪一種故障觸發的。m1 在三種半壞下都會出事，snapshot-decode 只在「回 200 但內容壞掉」時出事，
跟兩個補丁改的程式路徑完全對得上。

為此替故障代理加了一個功能：規則可以直接回自訂內容（`Rule.Body`），不必先卡住再放行，P2 才能
每一次查詢都回壞掉的 JSON。對應的測試是 `TestProxy_RuleCanAnswerWithAMalformedBody`。

過程中的錯誤：第一版腳本先檢查「故障送到幾次」再檢查「容器有沒有被停」。變異體一收到壞回應就停掉容器，
監看提早結束，只送到 1 次，結果被誤判成 INVALID。改成「容器被停且故障至少送到一次就是 FAIL」後，
三個版本全部重跑，上表是重跑的結果。

範圍限制：

- 每個版本只跑了一次（乾淨版本兩次）。
- 只有一個 node；跨 node 的搬移、斷電、重開機要上 VM 才測得到。
- 半壞只做了三種；server 回應很慢、只回一部分、Docker daemon 短暫沒回應都還沒做。

### VM 情境：預設設定下斷網，project 不能被轉走（2026-11-27，Hangar）

問題：ai-grace-config 錯在 `cmd/cara-server/main.go` 的接線（把 `cfg.GraceExpiredRelease` 寫死成 `true`）。
單元與整合測試直接建構元件、不跑 `main()`，結構上測不到。在真的機器上裝正式套件、真的斷網，抓不抓得到？

情境：`test/vm/scenarios/default-grace-partition.sh`，用 cara 自己的 VM 工具（`test/vm/run.sh`）在 Hangar 上跑。
3 台 VM（server、node-a、node-b，各 2 vCPU、4Gi），從 commit 建置正式套件、照運維方式安裝。
它是 `overlap-partition` 的對照版：那個情境開啟 `GRACE_EXPIRED_RELEASE`、證明 project 會被轉走；
這個用預設設定（關閉），只把 `RUNNING_GRACE_PERIOD` 縮成 1 分鐘以免等 3 分鐘。用 iptables 斷網，不需要強制斷電。

| 步驟 | 動作 | 預期 |
|---|---|---|
| S1 | project 在 node-a 跑起來 | — |
| S2 | 切斷 node-a 與 server 的網路 | node-a 變 NotReady |
| S3 | 等 server 做決定（要求授權，或把 project 轉走），再多等 60 秒 | 仍指派給 node-a、要求授權接管（`WorkloadStopUnconfirmed`）、node-b 上沒有副本 |
| S4 | 恢復網路 | project 在 node-a 繼續跑，只有一份 |

被測的兩個 commit：乾淨的 cara 是 `1f7f93f`（本分支），變異體是 `febe137`（從 `1f7f93f` 套上
`ai-grace-config.patch` 的本機分支 `st/mutant-ai-grace-config`，沒有推送）。

| 檢查 | 乾淨的 cara | ai-grace-config |
|---|---|---|
| S2 node-a 變 NotReady | 通過（115 秒） | 通過（101 秒） |
| S3 仍指派給 node-a | 通過 | **失敗**：被轉到 node-b |
| S3 要求授權接管 | 通過 | **失敗**：沒有這個 condition |
| S3 node-b 上沒有副本 | 通過 | **失敗**：node-b 上有 1 份 |
| S3 node-a 上的副本繼續跑 | 通過 | 通過（1 份） |
| S4 恢復後在 node-a 跑、只有一份 | 通過 | **失敗**：留在 node-b，node-a 的副本被清掉 |
| 合計 | 0 個失敗 | 6 個失敗 |

變異體的 S3 同時有「node-a 上 1 份」和「node-b 上 1 份」：**同一個 project 在兩台機器上同時跑**。
這正是預設關閉 grace-expired release 要防止的情況；如果 project 帶著 Managed 資料，兩份副本會各寫各的。

兩輪都只跑一次，各約 10 分鐘，結束後 VM 全部刪除（`hangar get vm` 確認是空的）。
紀錄（含 server 與 agent 的 journal）在 repo 外、hank 機器上的 `../test-records/2026-11-27-st-default-grace-1f7f93f/`
與 `../test-records/2026-11-27-st-default-grace-mutant-febe137/`（沒有放進 repo）。

意義：這是第一個「只有真實環境測試抓得到」的變異體被真實環境測試抓到的實例。單元、整合測試結構上碰不到
`main.go`；前一節的 Codex 測試是在測試程式裡呼叫 `main()`，這裡則是裝真的套件、跑真的 systemd 服務、真的斷網。

### 時間縮放 PoC：e2e 快 4～5 倍，抓錯能力不變（2026-11-27）

問題：cara 的 e2e 與 VM 情境，多數時間花在等寫死的期限（失聯門檻 90 秒、寬限期 3 分鐘、刪除孤兒前等 3 分鐘、
輪詢 10 秒……）。把期限縮短能不能讓測試變快，而且不會因此漏抓錯誤？

做法：`test/injection/timescale.patch`。新增 `internal/timescale`，用環境變數 `CARA_TIME_SCALE`（0～1）把
失聯、轉移、清理、復原相關的期限**按同一個比例**縮短：server 10 個（失聯門檻、健康檢查週期、寬限期、刪除逾時、
四個控制器的重掃週期、重排間隔與錯誤退避），agent 9 個（預設心跳間隔、輪詢週期、刪除孤兒前的等待、兩個復原期限、
兩段復原退避、重新註冊的起始與最大退避）。備份、還原、HTTP 逾時不縮放，因為那些是真實工作或網路需要的時間。

同比例縮放是重點：期限之間有大小關係（心跳間隔 < 失聯門檻 < 寬限期），只縮其中幾個就會測到一個現實不存在的
系統。前一節的觀察 2 就是例子。

補丁只在建置情境時套上（跟變異體一樣），repo 的產品碼不變。套上補丁但不設 `CARA_TIME_SCALE` 時，
`internal/agent`、`internal/server/controller`、`internal/config` 的單元測試照樣全過。

兩個情境支援縮放：
- 本專題的 `server-degraded.sh` 加了 `--timescale`，故障視窗按同一個比例縮。
- cara 自己的 `scripts/e2e/orphan-cleanup.sh` 由 `test/injection/timescale-orphan-cleanup.sh` 包起來執行：
  在暫存副本裡套補丁，把唯一一個固定等待 `sleep 120`（3 分鐘寬限期的三分之二）按比例縮短；腳本本身不改。

為了讓 orphan-cleanup 有一個能殺的變異體，新增 `t1-orphan-removed-without-grace.patch`：停掉孤兒後不等寬限期
就刪除。

**速度**（同一台機器、依序執行、不平行）：

| 情境 | 原速 | 縮成 0.1 | 加快 |
|---|---|---|---|
| server-degraded | 230 秒（準備 21 + 情境 209） | 53 秒（準備 21 + 情境 32） | 4.3 倍；情境本身 6.5 倍 |
| orphan-cleanup | 232 秒 | 49 秒 | 4.7 倍 |

剩下的時間主要是固定成本：啟動 PostgreSQL、Docker-in-Docker 與建置。

**抓錯能力**：

| 情境 | 變異體 | 原速 | 縮成 0.1 |
|---|---|---|---|
| server-degraded | m1 | P1、P2、P3 失敗 | P1、P2、P3 失敗 |
| server-degraded | ai-snapshot-decode | 只有 P2 失敗 | 只有 P2 失敗 |
| orphan-cleanup | t1 | 失敗：「寬限期到之前就被刪了」（160 秒） | 同樣的失敗（40 秒） |

（server-degraded 原速的兩個變異體結果來自前面的單機 PoC 那一節。）三個變異體縮放前後的判定完全一樣，
連失敗在哪一段都一樣。

**穩定性**：縮成 0.1 的乾淨版本，兩個情境各另外重跑 3 次，加上第一次共 4 次，全部通過；耗時分別是
52～55 秒與 48～49 秒。

範圍限制：
- 只有兩個情境、三個變異體；VM 情境還沒縮放（VM 的期限也會受縮放影響，但 3 台 VM 的開機與安裝約 5 分鐘是
  固定成本）。
- 縮成 0.1 時輪詢間隔是 1 秒、失聯門檻 9 秒；更小的倍數會讓期限逼近容器啟動與網路延遲，還沒量過極限在哪。
- 判斷「抓錯能力不變」只看了 pass/fail；邊界型變異體（剛好等於期限的那一瞬間，例如 s2、s4、s5）本來就殺不掉，
  縮放也不會改變這點。

### 對最新 main 找真 bug：沒有找到，但有兩個值得記的觀察（2026-11-27）

對象：`origin/main` = `88b3482`。它比本分支的基準 `2c88557` 只多 4 個 commit，都是 caractl 的改動，
所以重跑既有情境意義不大；改成瞄準**現在沒有任何測試做過**的情況。

**單機（真的執行檔，`test/injection/scenarios/disruptions.sh`）**，用兩種 server 設定各跑一次：預設，以及
`GRACE_EXPIRED_RELEASE=true RUNNING_GRACE_PERIOD=1m`（cara 自己的 `overlap-partition` 用的設定）。
node-b 是腳本代為心跳的假 node，讓錯誤的轉移有地方去、會顯示成 nodeRef 改變。

| 情境 | 內容 | 預設 | 寬限期到就轉、1 分鐘 |
|---|---|---|---|
| D1 | cara-server 停 240 秒（超過 90 秒失聯門檻）再啟動，觀察 180 秒 | 通過 | 通過 |
| D2 | server 停機期間 agent 重啟（整個機房斷電、node 比 server 先開機） | 通過 | 通過 |
| D3 | 刪掉 project 後用同名重建（缺陷紀錄第 26 項曾經壞在這裡） | 通過（11 秒） | 通過（11 秒） |

**VM（Hangar，`test/vm/scenarios/upgrade-from-rc1.sh`）**：先裝 `v0.1.0-rc.1` 的正式套件、跑兩個 project
（一個帶 Managed 資料），再照運維順序**先升 server、再升 agent**，中間的混合版本期間也建一個新 project。
從 rc.1 到 main 有 25 個 commit，包含 node 授權（CARA-140）、spec 版本（新 migration）、Managed project
預設每天備份等。結果 0 個失敗：升級 server 後舊 agent 照常運作、容器沒被動到；混合版本期間能建新 project；
升級 agent 後全部 project 照跑、Managed 資料還在；升級後改 spec、刪 project 都正常。
紀錄在 `../test-records/2026-11-27-st-upgrade-rc1-to-88b3482/`。

結論：**這一輪沒有找到 cara 的 bug。** 兩個觀察：

1. **升級 agent 會把所有工作負載同時重啟一次。** 新 agent 把 rc.1 建的每個容器都重建了（Managed project 的
   `/data/boots` 從 1 筆變 2 筆）。原因是 ADR-0026 規定「沒有 `cara.spec-hash` label 的容器視為過期」，
   rc.1 的容器沒有這個 label。這是設計上的結果，不是 bug；但 ADR-0033 只說升級會重啟 cara 的服務，沒提到
   使用者的服務也會一起重啟，升級說明應該寫出來。另外，只要以後改了 hash 的算法、或替 service 加一個
   預設不是空值的欄位，每次升級都會重建所有容器；這個情境的 `U4 ... kept by the new agent` 會把它記下來。
2. **server 重啟後會短暫誤判健康的 node。** D1 裡 server 一啟動就把心跳太舊的 node 標成 NotReady，並替
   所有 running project 啟動失聯計時器，直到下一次心跳才解除（這次約 10 秒；agent 預設 30 秒心跳一次，
   最長約 30 秒）。只要這段時間短於寬限期就沒有傷害；但如果運維把寬限期設得比心跳間隔還短，server 每次
   重啟都可能把健康 node 上的 project 轉走。cara 沒有檢查這兩個設定的大小關係。

範圍限制：每個情境只跑一次；VM 只跑了升級一個情境；斷電、跨 node 搬移中途重啟等還沒做。

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
