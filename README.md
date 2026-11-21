# opencrab-bluesky-gateway

OpenCrab のエージェントを Bluesky アカウントにつなぐゲートウェイです（Go）。

- エージェントごとに Bluesky アカウント（ハンドル＋アプリパスワード）を設定します。1 つのプロセスで何体でも動かせます。
- アカウント宛てのメンション・返信・引用を、OpenCrab core の会話（セッションキー `bluesky-<エージェントID>`）へ渡します。
- エージェントは投稿・返信・いいね・リポスト・フォローができます。ツールを使わずに書いた本文は、新しい投稿になります。
- 設定画面を自分で出します（既定 `0.0.0.0:8795`）。

core とは core の gate ソケット（Unix ドメインソケット）で、gate wire protocol 3 を話します。資格情報は core に渡しません。アカウントの情報はこのゲートウェイだけが持ちます。

## 動き

### 受信

- `app.bsky.notification.listNotifications` を、bot ごとの間隔（既定 15 秒）で読みます。
- 理由が `mention`・`reply`・`quote` の通知だけを渡します。自分の投稿、いいね、フォロー、リポストの通知は渡しません。
- core へ渡す内容：
  - `origin` と `ref` は投稿の at:// URI。エージェントの会話では eN 番号になります。
  - `author_id` は相手の DID。表示名は「表示名 (@ハンドル)」です。
  - 画像は CDN の https URL です。
  - 返信なら、`reply_target` に返信先の投稿を入れます。
- オーナーに入れたハンドルか DID からの投稿は、オーナーの発言として渡します。
- どこまで渡したかを保存します（最後の時刻と、最近の URI 500 件）。再起動しても、同じ通知を二度渡すことも、取りこぼすこともありません。最初の起動では、それより前の通知は渡しません。

### 送信（エージェントが使える操作）

| 操作 | 引数 | 内容 |
|---|---|---|
| `post` | `text`、`files`（任意） | 新しい投稿。300 文字（書記素）を超えると、スレッドに分けて投稿します。 |
| `reply` | `event`（e番号か at:// の uri）、`text`、`files`（任意） | 返信。スレッドの root と parent を正しく付けます。 |
| `like` | `event`（e番号か at:// の uri） | いいね |
| `repost` | `event`（e番号か at:// の uri） | リポスト |
| `follow` | `user`（u番号、ハンドル、DID） | フォロー |
| `send_dm` | `text` | DM の会話の中でだけ使えます（下の「DM」）。 |

- `files` は、エージェントの作業フォルダのファイルです。画像（JPEG・PNG・WebP・GIF）4 枚まで、または動画（mp4）1 本のどちらかです。画像は 2MB を超えると自動で縮めます。動画は 300MB・10 分まで（app.bsky.embed.video の上限）で、Bluesky の動画サービス（video.bsky.app）に送って処理を待ってから投稿します。`attachment_outbox_root` を設定したときだけ使えます。
- 本文の URL と、`@ハンドル`（DID が引けるもの）は、リンクとして表示されます。
- `event`・`user`・`ref` のように会話の番号（e番号・u番号）を受ける欄には、操作の宣言（`input_schema`）で `"x-opencrab-short-ref": true` を付けています。core はこの欄だけ、番号を実際の at:// URI や DID に置き換えてから渡します。`files` の要素には `"x-opencrab-workspace-file": true` を付け、core が作業フォルダのファイルを送信用の置き場に写してから渡します。どちらも core 用の目印で、JSON Schema の `format` は使いません（使うと、モデルによっては道具の説明ごと断られます）。

### DM（オーナーだけ）

- オーナー（設定の「オーナー」、ハンドルか did）から届いた DM だけを、会話ごとの session `bluesky-dm-<agent_id>-<convoId>` へ渡す（caller はオーナー）。オーナー以外の DM と自分の DM は渡さない。
- DM は `chat.bsky.convo.getLog` を通知と同じ周期で読む。読んだ位置は保存するので、再起動しても二重にも抜けにもならない。初回は「今」から読む。
- DM の session で書いた地の文は、そのDMに `chat.bsky.convo.sendMessage` で送られる。操作 `send_dm`（`text`）も同じ送り先（サブタスクからも使える）。
- DM の呼び出しは PDS 経由で、ヘッダ `atproto-proxy: did:web:api.bsky.chat#bsky_chat` を付ける。
- OAuth では `transition:chat.bsky` の許可が要る（このスコープを要求する）。古いログインは設定画面に「DM: 再ログインが必要」と出るので、「もう一度ログイン」を押す。アプリパスワードの場合は、作るときに「DM へのアクセスを許可」を付けたものが要る。

### 読む（エージェントが自分で呼ぶ操作）

ハートビートや定期実行のターンで、エージェントが自分で呼んで読みます。結果は同じターンでエージェントに返ります。何も投稿しません。

| 操作 | 引数 | 内容 |
|---|---|---|
| `read_timeline` | `limit`、`cursor`（どちらも任意） | 自分のホームタイムライン |
| `read_author_feed` | `user`（u番号、ハンドル、DID）、`limit`、`cursor` | ある人の投稿 |
| `search_posts` | `query`、`limit`、`cursor` | 投稿の検索 |
| `read_thread` | `event`（e番号か at:// の uri）、`depth`（既定 6、最大 10） | スレッド（親と返信。本文は省略しない） |
| `resolve` | `ref`（e番号、at:// の uri、bsky.app の投稿 URL） | 1 つの投稿の詳細（本文は全文、返信先とスレッドの最初の uri、引用先の uri、画像の URL） |

- 投稿ごとに、作者、時刻（UTC）、返信・リポスト・いいねの数、本文、画像の https URL、at:// の uri を返します。結果の `images` に画像の URL を並べます（core の `view_image` に渡すと画像を見られます）。uri は `reply`・`like`・`repost` の `event` にそのまま渡せます。
- 一覧の本文は 150 文字（書記素）までです。全文は `read_thread` で読みます。
- `limit` の既定は 8 件（最大 100）です。core はゲートウェイの操作の結果を約 2,500 トークンまで会話に入れ、超えるとファイルに保存して場所だけを返します。既定の件数はそこに収まる目安です。続きは `next_cursor` を `cursor` に渡して読みます。

## 設定ファイル

```json
{
  "core_socket": "/path/to/opencrab/data/run/gate.sock",
  "data_dir": "/path/to/bluesky-gateway-data",
  "listen": "0.0.0.0:8795",
  "core_api": "http://127.0.0.1:8080",
  "attachment_outbox_root": "/path/to/opencrab/data/attachments/outbox",
  "oauth": {
    "client_id": "https://<your-pages>/client-metadata.json",
    "callback_url": "https://<your-host>:8796/oauth/callback"
  }
}
```

| キー | 必須 | 内容 |
|---|---|---|
| `core_socket` | ○ | core の gate ソケット（core 設定の `[gate] listen_socket`） |
| `data_dir` | ○ | bot の設定と受信の位置を置くフォルダ。`bots.json` はモード 0600 で書きます。 |
| `listen` |  | 設定画面の待ち受け。既定 `0.0.0.0:8795` |
| `core_api` |  | core の HTTP API。設定画面のエージェント一覧に使います。無ければエージェント ID を手で入れます。 |
| `attachment_outbox_root` |  | core の送信用ファイル置き場。画像と動画の投稿に使います。 |
| `oauth` |  | 「Bluesky でログイン」を使うときの設定（下の「OAuth でログイン」）。無ければアプリパスワードだけです。 |

## 動かす

```sh
go build -o opencrab-bluesky-gateway ./cmd/opencrab-bluesky-gateway
./opencrab-bluesky-gateway config.json
```

### 常駐させる

本番での常駐と入れ替えは、このリポジトリの外の配備の仕組みが行います。このリポジトリが決めているのは、次の受け渡しの形だけです。

- パッケージは、ディレクトリ 1 つの中に `commit`（40 桁のコミット）と `bin/opencrab-bluesky-gateway` を入れた tar.gz です。
- 起動は `opencrab-bluesky-gateway <設定ファイル>` です。設定ファイルの項目は上の「設定ファイル」のとおりです。
- 起動できたかは、設定画面（`listen` のポート）が応答することと、動かす設定の bot の数だけ core への `hello ok` がログに出ることで確かめられます。

`deploy/com.kojira.opencrab-bluesky-gateway.plist`（launchd の LaunchAgent）は、以前の常駐方法の例です。今の本番では使っていません。

## 設定画面

`http://<ホスト>:8795/` を開きます。

1. 「bot を追加」でエージェントを選んで、「追加」を押します。この時点では「未設定」です。
2. その bot の欄に、次を入れて「保存」します。
   - ハンドル（例 `example.bsky.social`）
   - アプリパスワード：Bluesky の「設定 → プライバシーとセキュリティ → アプリパスワード」で作ります。
3. 状態が「動作中」になれば、つながっています。ログインできないときは「エラー」になり、理由が出ます。

その他：
- 保存すると、その bot だけが再起動します。
- アプリパスワードは画面にも API にも出しません（「設定済み」と出ます）。変えるときだけ入れ直してください。
- 「動かす」を外すと止まります。「削除」すると bot の設定が消えます（core の会話の記録は残ります）。

## OAuth でログイン（Bluesky でログイン）

アプリパスワードの代わりに、Bluesky の画面でログインして bot とアカウントを紐づけられます。

使い方（設定画面）：
1. bot の欄にハンドルを入れて「Bluesky でログイン」を押します。
2. Bluesky の画面に移るので、その bot のアカウントでログインして「許可」します。
3. 設定画面に戻り、「ログイン済み（OAuth）」と出ます。アプリパスワードは要りません。
4. やめるときは「ログアウト」を押します（トークンを取り消して、消します）。

- 取ったトークンと鍵は `data_dir` の `oauth.json`（モード 0600）にだけ置きます。画面・API・ログには出しません。
- ログインしたアカウントが、ログインを始めたハンドルのアカウントと違えば断ります。
- ハンドルを変えると、OAuth のログインは解除されます（新しいアカウントで入り直してください）。
- この gateway は「confidential client」です。署名鍵は初回起動時に `data_dir/oauth-client-key`（モード 0600）に作り、公開鍵だけをクライアントメタデータに載せます。鍵を消すと、ログインし直しになります。

### 準備（gateway ごとに 1 回）

OAuth のクライアント（`client_id`・戻り先・署名鍵）は、gateway を動かす人がそれぞれ自分で用意します。このリポジトリには、だれかのクライアントは入っていません。

Bluesky の認可サーバーは、`client_id` の URL からクライアントメタデータ（JSON）をインターネット越しに読みに来ます。戻り先（`callback_url`）はブラウザが開ければよいので、手元のネットワークの中だけで構いません。

1. 戻り先を https にします。設定画面が `8795` なら、たとえば Tailscale で `8796` を https にします。

   ```sh
   tailscale serve --bg --https=8796 http://127.0.0.1:8795
   ```

   `callback_url` は `https://<your-host>:8796/oauth/callback` です（`<your-host>` はそのマシンの名前）。設定画面もこの https の URL で開けます。
2. メタデータを公開する https の URL を決めます（例 `https://<your-pages>/client-metadata.json`。GitHub Pages など、インターネットから読めるところ）。この URL が `client_id` です。
3. 設定ファイルの `oauth` に `client_id` と `callback_url` を書いて起動します。署名鍵は初回起動時に `data_dir/oauth-client-key` に作られます。
4. `https://<your-host>:8796/oauth/client-metadata.json` に出る JSON を取り、2 で決めた URL にそのまま置きます。
   - 中身は `client_id`・戻り先・公開鍵などです。秘密は入っていません。
   - `client_id` は、その JSON を取る URL と完全に同じでなければなりません。
   - 署名鍵・`client_id`・`callback_url` のどれかを変えたら、もう一度取り直して置き直します。

## 開発

```sh
go vet ./...
go test -race ./...
```

## ライセンス

MIT
