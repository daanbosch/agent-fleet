# 119. Managed opencode の呼び出し元セッションをプラグインで届ける（#989）

- 依頼: #989（#978 の案 b を切り出したもの）。Managed opencode は af MCP 子に「どのセッションか」を
  伝える経路が無く、セッションに結び付く af ツールは cwd と生死の推定に落ちる。同じ worktree で
  2 本が動いていると曖昧として断る。
- 関連: [117](117-managed-af-session-name-delivery.md)（他 kind の経路と、opencode に設定の口が
  無いことの再測定）/ [27](27-agent-managed-driver.md) §9.3.1。
- 版: opencode 1.18.32。

## 測り方（課金なし）

偽の OpenAI 互換エンドポイント（決まった tool_call を返すだけ）を custom provider として登録し、
本物の `opencode serve` に Python の stdio MCP（受け取った `tools/call` の引数と cwd を記録する）を
2 つ繋いだ。1 つは af と同じ形の名前（`af_<8 桁 16 進>`）、もう 1 つは無関係な名前にした。
同じディレクトリに 2 セッションを作り、1 ターンずつ回した。

## 実測結果

1. **`tool.execute.before` は MCP ツールでも発火**し、`input.sessionID` は opencode のセッション ID。
   `output.args` に足した鍵は、MCP 子の `tools/call` の `arguments` にそのまま届いた。
   同じ子を共有する 2 セッションは、それぞれ自分の ID を届けた。
2. **スキーマ検査で弾かれない。** af のツールは `additionalProperties: false` だが、検査はモデル
   出力に対して先に済んでおり、フックはその後に動く。モデル自身が同名の鍵を書いた場合も弾かれず、
   プラグインがあれば上書きされ、**無ければ書いた値がそのまま届く**。
3. 🔥 **足した鍵は opencode のツール記録（`state.input`）に残り、次のターンでモデルに返る**
   （assistant の `tool_calls.arguments`）。`tool.execute.after` で消しても記録は書き込み済みで
   手遅れだった。AF のミラーはツール入力から決まった鍵（command / path など）しか表示しないので、
   画面には出ない。モデルが鍵を覚えて自分で書いても、2 のとおり上書きされる。
4. MCP 子の cwd はセッションのディレクトリ（既存の cwd 推定が前提にしていたことの確認）。

## 実装

- `workspace/opencode-plugin/agent-fleet-caller.js`: ツール名が `^af_[0-9a-f]{8}_` のときだけ
  `args._af_caller_sid = input.sessionID` を**上書き**する。他社の MCP には触らない。旧名の素の
  `af` は `af_` だけでは他のツールと区別できないので対象外（cwd 推定のまま）。entrypoint が起動
  ごとに `*.js` をコピーするので、配布と更新は `agent-fleet-status.js` と同じ経路に乗る。
- `mcpx.mcpStdioCall`: 引数から `_af_caller_sid` を取り除いてから処理する（メモ系は引数を CP へ
  そのまま中継するため、先に除く必要がある）。値は 1 回の呼び出しの間だけ持つ（処理は直列）。
- `mcpx.mcpOwningSession` の順:
  1. `AF_SESSION_NAME`（プロセス単位の事実。呼び出しごとの申告では上書きしない）
  2. `mcpCallerSession`: 作業フォルダ（`Dir` か `CWD()`）が一致し、アーカイブされていない
     opencode セッションのうち、AF の sid ストア（`opencode.SlotSessionID`、Managed ドライバが
     書く）の値が申告と一致するものが**ちょうど 1 つ**あり、しかも Agent が生きていると答えたとき
     だけ採る
  3. 今までの cwd と生死の推定
- 🔥 **tools/list には申告が乗らない。** `generate_image` は一覧を作る時点で持ち主を解決し、
  解決できなければ一覧に出さない（`mcpImageGenAdvertise`）。呼び出し側だけ直しても、同じ worktree
  に 2 本いると道具そのものが見えず、受け入れ条件を満たせない。そこで、フォルダの生きている
  セッションが**全部 Managed opencode**（＝どの呼び出しも申告を運べる）のときに限り、スタジオでない
  1 本を代表にして一覧に出し、誰のものかは呼び出しの申告で決める（`mcpStampedFolderSessions`）。
  スタジオの道具が「持ち主が見分けられないなら出して、呼び出しで理由付きで断る」（ADR 0100）のと
  同じ形。共有された子の一覧はもともと全セッションで 1 つなので、1 本ごとに変えることはできない。
  他 kind が混ざる・生死が読めない・全員スタジオ、のときは今までどおり出さない。
- 信頼: 申告はモデルでも書ける（プラグインは利用者が消せる）ので、セッション名としては受け取らず、
  AF の対応表を引く鍵としてだけ使う。一致しなければ黙って 3 に落ちる。つまりプラグインが無い・
  壊れている状態は、今までと同じ結果になる。

## 検証

- 単体（`mcp_stdio_caller_test.go`）: 同じフォルダの 2 本がそれぞれ自分に解決される／偽造・停止中・
  別フォルダ・opencode 以外の申告は採らない／一致しない申告は cwd 推定の答えを保つ／subdir／
  `AF_SESSION_NAME` が勝つ／鍵が Agent へ渡らず次の呼び出しに残らない／同じフォルダの 2 本に
  `generate_image` が一覧に出て、生成はそれぞれ申告した側に付く（申告なしは断る）／他 kind 混在・
  生死不明・全員スタジオでは一覧に出ない。変異 11 本のうち 10 本はどれかのテストが落とした。
  残る 1 本（一覧の代表選びで生死の読み取り失敗を無視する）は、`mcpAliveSessions` が失敗時に空を
  返すので挙動が変わらない等価変異。
- 契約（`contract_caller_plugin_test.go`、`-tags clicontract`、tier A）: 上の 1・2・4 を出荷する
  プラグインそのもので固定した。陰性対照として、全ツールに付けるプラグインと何もしないプラグインの
  どちらでも赤になることを確かめた。
- 残り: 配備後に実 Managed opencode 2 本を同じ worktree で動かし、`propose_session_handoff` /
  `generate_image` がそれぞれ自分に解決されること（#989 の受け入れ条件）。

## レビュー（codex の子セッション）で直したこと

- 🔴 **一覧の監視 goroutine が呼び出しの申告を読んでいた。** `mcpWatchToolList` は 1 分ごとに
  `mcpStdioToolList` を別 goroutine で組み直し、その中の `mcpImageGenAdvertise` /
  `mcpStudioAdvertise` が `mcpOwningSession` → `mcpCallerSID` を読む。`generate_image` の呼び出しは
  最長 33 分ループを占有するので、その間の監視は**データ競合**になり、しかも共有された子の一覧を
  「たまたま呼んでいた 1 本」の持ち主で組み直す。持ち主の解決を呼び出し用（申告を使う）と一覧用
  （`mcpListOwningSession`、申告を見ない）に分けた。`-race` で、一覧側を申告に戻す変異が
  `mcpCallerSession` の競合として検出されることを確かめた（CI は `-race` を使わないので、CI での
  見張りは意味を見るテストの方）。
- 🔴 **プラグインが印を付けない状況で、モデルの書いた値を信じていた。** プラグインが消された・
  af が旧名の `af`（プラグインは乱数付きの名前しか見ない）のとき、モデルが同じフォルダの生きている
  別セッションの `ses_` を書けばそのセッションとして扱われた。申告は「プラグインが入っている かつ
  af の名前が乱数付き」のときだけ使う（`mcpCallerStampTrusted`）。モデルは同じ uid でシェルを持つ
  ので、`AF_SESSION_NAME` を偽って `mcp-stdio` を自分で起動することもでき、これは壁ではない。
  予約引数を「普通のツール呼び出しで開く扉」にしない、というところまで。
- 🟡 **一覧に出したのに毎回断られる形があった。** 一覧側も同じ信頼条件と、生きている全員に sid
  ストアの対応があることを求める。
- 変異 6 本（一覧が申告を読む・常に信頼・対応の確認なし・名前の確認なし・プラグインの確認なし・
  呼び出し側が信頼条件を見ない）はすべてどれかのテストが落とした。

## 再レビューで直したこと

- 🔴 **信頼条件がプラグインのファイルの「存在」しか見ていなかった。** 空・別の JS に置き換えられて
  いても申告を信じた。イメージ同梱の原本（`/usr/local/share/agent-fleet/opencode-plugin/`）と
  バイト一致するときだけ信じる（`opencode.CallerPluginCurrent`）。この開発環境のように、イメージが
  まだプラグインを持たない版なら信じない＝cwd 推定のまま。
- 🟡 **`XDG_CONFIG_HOME`**: opencode はこれを見るが、af の書き込みはすべて HOME 基準
  （`paths.OpencodeConfigDir`）。別の場所を指しているときは、確かめたファイルが opencode の読む
  ものではないので信じない。
- 🟡 **生きている 2 本が同じ `ses_` に対応していると、一覧に出して毎回断る**形が残っていた。
  一覧側は対応が互いに異なることも求める。
- 🔴（既存の穴）**subdir で起動したセッションが、作業フォルダの推定とスタジオの一覧判定から
  漏れていた。** どちらも `Meta.Dir` だけを比べており、subdir で走る MCP 子の cwd と一致しない。
  今回の変更で入ったものではないが、`mcpRunsIn`（`Dir` か `CWD()`）にそろえた。
- 指摘のうち「Agent 再起動で af 名が回ると、引き継がれた旧 `opencode serve` の子の申告を全部拒否
  する」は再現しない。信頼条件は「旧名の素の `af` でない」ことで、現在の名前との一致は見ていない。
  旧名も `af_<8 桁>` ならプラグインは印を付ける。
- 変異 5 本（内容の比較なし・XDG の確認なし・重複の確認なし・推定が root だけ・スタジオが root
  だけ）はすべてどれかのテストが落とした。

## 3 巡目のレビューで直したこと

- 🔴 **信頼条件が見ていたのは「Agent の現在の af 名」で、この子が実際に登録されている名前では
  なかった。** プロジェクトの `opencode.json` などが同じコマンドを素の `af` で登録していると、
  プラグインは印を付けない（乱数付きの名前しか見ない）のに、子は信頼条件を満たしてモデルの書いた
  値を信じた。opencode は子に自分の登録名を教えないので、af の opencode エントリだけに
  `environment.AF_MCP_SERVER_KEY=<登録名>` を書き（`mcpreg.OpencodeServers`）、子はこれが
  `af_<8 桁>` の形のときだけ申告を使う。起動引数は変えない（全 kind の af 定義と、それを比べる
  テストに波及するため）。副作用として、Agent 再起動で名前が回っても、引き継がれた旧 serve の子は
  自分が起動された時の名前で判定される（2 巡目で「再現しない」とした件は、この形でも成り立つ）。
- 変異 2 本（鍵を書かない・鍵を見ない）はどちらもテストが落とした。

## 4 巡目のレビュー: 直さないと決めたこと

- 指摘: プロジェクトの `opencode.json` が素の `af` で同じコマンドを登録し、`environment` に
  `AF_MCP_SERVER_KEY=af_<8 桁>` を自分で書くと、プラグインは印を付けないのに子は申告を信じる。
- 経路は実在するが塞がない。MCP エントリの書き手は子の環境を丸ごと決められ、その書き手は
  `AF_SESSION_NAME=<他人>` を書くだけで、申告と関係なく以前からなりすませる
  （`mcpOwningSession` は `AF_SESSION_NAME` を最初に無条件で信じる）。環境で届ける身元は、それを
  書く設定より強くならない。この検査が閉じるのは「誰も嘘をつくつもりが無いのにずれる」形
  （旧名・残骸・プラグインの欠落や改変・XDG）まで。
- 「親の環境から継承する」形は経路が無い。`AF_MCP_SERVER_KEY` を持つのは opencode が起動した af
  子だけで、af 子は opencode を起動せず、モデルのシェルは opencode が起動する。

## 範囲外

- opencode の task ツールで作られる子セッションは `ses_` が別なので一致せず、cwd 推定に落ちる
  （悪化はしない）。親をたどるなら opencode のストアの `parent_id` が使える。
- Terminal の opencode は `AF_SESSION_NAME` が先に決まるので、プラグインの申告は除かれるだけ。

## 2026-10-14: opencode 2 (#2104, part of #2101)

Measured on `@opencode/cli` 2.0.28 and `opencode-ai` 1.19.0, each in an isolated `HOME`, against a
stub OpenAI-compatible endpoint and a stub MCP server (no real model).

- The three plugins are now one module per file: a plain default-exported
  `{ id, setup(ctx), server() }`. 2.0.28 loads it from `~/.config/opencode/plugin/` (and from
  `plugins/`) without `@opencode/plugin`, `.ts` included, and ignores `server`; 1.19.0 calls
  `server`. The directory stays `plugin/`, so `CallerPluginCurrent` and the entrypoint seed are
  unchanged and no stale file can collide on an id. `/api/plugin` lists the ids.
- `ctx.tool.hook("execute.before")` fires for the model's `shell` call and, separately, for each
  MCP call made inside the Code Mode `execute` tool (tool name `af_<hex>_<tool>`, with
  `sessionID`). Mutating `event.input` in place reaches the tool: the rtk rewrite took effect.
  `POST /api/session/{id}/shell` (the user's own shell) does not fire it.
- **The stamped argument does not survive on 2.0.28 when the tool's `inputSchema` is closed**
  (`additionalProperties: false`, as af's are); with an open schema it arrives. Every 2.0.28
  `tools/call` carries `params._meta["ai.opencode/sessionID"]` (1.19.0 sends none), so
  `mcpStdioCall` now takes that as a second source; the stamp still wins, and `_meta` needs only the
  rotated server key, not the plugin, since the model cannot write it.
- Status events: `session.execution.started` → working; `succeeded`, `failed` and `interrupted`
  → idle (each observed; `session.idle` was not emitted). Subagent sessions carry `parentID` on
  `session.created` and are left out so their end does not flip the badge.
- Not covered: a Terminal (CLI) opencode 2 shares one background service, so
  `AF_SESSION_SID` in the plugin's environment is the service starter's (#2105).

## 2026-10-14: opencode 2 TUI launch, service, install (#2105, part of #2101)

Measured on `@opencode/cli` 2.0.28 in an isolated `HOME`, a loopback stub OpenAI-compatible
provider with an explicit stub model, service port moved off 49374.

**Shared service.** A bare `opencode` (and `opencode models`, `run`, `session list`) starts
`opencode serve --service`, one per user, and that process keeps the environment of whoever
started it (`AF_SESSION_SID` of the first session was in its `/proc/<pid>/environ`). Per-session
env therefore never reaches a plugin or a provider on a second session. `opencode service set
disabled true` (file `~/.config/opencode/service.json`; keys: disabled, remote, hostname, port,
password, cors, env) makes every command run a private `serve --stdio --port 0` instead: no
service process afterwards. Decision: **`--standalone` per TUI** rather than `--server` to af's
serve.

| layout (idle, RSS) | per TUI | server | 3 sessions |
|---|---|---|---|
| `--standalone` | ~160 MB | ~245 MB each | ~1.2 GB |
| shared service or `--server` to one serve | ~160 MB | ~300 MB once | ~0.8 GB |

`--server` works (`OPENCODE_SERVER_PASSWORD` in the TUI's environment carries the password), but
the server's environment, not the TUI's, is what plugins see, so the status badge, slot-id capture
and the session-scoped engine token would break, and Terminal sessions would have to count as
daemon dependents and survive a serve restart. The price of `--standalone` is ~245 MB per Terminal
session; v1's TUI embeds its server in-process, so this is the v2 equivalent.

**Model, agent, fork.** `--model/--agent/--fork` are not TUI flags. `OPENCODE_CONFIG_CONTENT`
(`model`, `default_agent`) picks them for a *new* session; a session that exists keeps its own
whatever the config says. `opencode api --standalone POST /api/session/{id}/model|agent|fork`
(~1 s each) works against the same database, so a resume sets them before attaching and a fork is
made first and the copy resumed. Forking an empty session is refused by v2.

**Config.** `OPENCODE_CONFIG` (a file path) is ignored by v2; `OPENCODE_CONFIG_CONTENT` and the
project `opencode.json` are read. The chat read-only policy (`permission: {edit, bash: deny}`, v1
shape) held in a project `opencode.json` on v2 (the shell tool is removed from the model's list) but
not when delivered through `OPENCODE_CONFIG`, so chat and one-shot runs now also pass the same JSON
inline on v2. Instructions: the global `~/.config/opencode/AGENTS.md` is read (fleet policy and
memory guide arrive); `CLAUDE.md` (project, global, `~/.claude`) and the `instructions[]` array are
not. The member's own instructions are therefore composed into that AGENTS.md as the `user-notes`
block on v2 and kept in `instructions[]` on v1.

**Install.** `@opencode/cli` installs `opencode` and `opencode2` bins (the same executable), and
`opencode-ai` and it both claim `opencode`, so a lean-variant boot that crosses the major
uninstalls the other package first. The auto-updater is `update: "disable"` in the config, seeded
by the entrypoint when the installed major is 2.

Nothing was run against a real provider. The pin stays 1.19.0.

Review round 1 (#2118): a top-level `permission` deny is only a rule, and a later project or agent
rule (`agents.build.permissions` allow) brings shell/edit/write back (a shell call created a file).
`experimental.policies` entries (`tool.use`, resource `<action>:*`, deny) are evaluated after every
rule and held (`Blocked by configuration policy`) with the same agent allow in place; a bare tool
name as the resource matched nothing. Chat and one-shot runs on v2 now carry `shell:*`, `edit:*`,
`write:*` and `subagent:*` denies inline. Code Mode's `execute` did not reach the built-in shell
even without a policy (control run), so it has no entry of its own.
