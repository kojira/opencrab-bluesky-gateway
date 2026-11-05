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

- 本文の URL と、`@ハンドル`（DID が引けるもの）は、リンクとして表示されます。
### 読む（エージェントが自分で呼ぶ操作）

ハートビートや定期実行のターンで、エージェントが自分で呼んで読みます。結果は同じターンでエージェントに返ります。何も投稿しません。

| 操作 | 引数 | 内容 |
|---|---|---|
| `read_timeline` | `limit`、`cursor`（どちらも任意） | 自分のホームタイムライン |
| `read_author_feed` | `user`（u番号、ハンドル、DID）、`limit`、`cursor` | ある人の投稿 |
| `search_posts` | `query`、`limit`、`cursor` | 投稿の検索 |
| `read_thread` | `event`（e番号か at:// の uri）、`depth` | スレッド（親と返信。本文は省略しない） |

- 投稿ごとに、作者、時刻（UTC）、返信・リポスト・いいねの数、本文、at:// の uri を返します。uri は `reply`・`like`・`repost` の `event` にそのまま渡せます。
- 一覧の本文は 150 文字（書記素）までです。全文は `read_thread` で読みます。
- `limit` の既定は 8 件（最大 100）です。core はゲートウェイの操作の結果を約 2,500 トークンまで会話に入れ、超えるとファイルに保存して場所だけを返します。既定の件数はそこに収まる目安です。続きは `next_cursor` を `cursor` に渡して読みます。

- `files` は、エージェントの作業フォルダの画像です（JPEG・PNG・WebP・GIF、4 枚まで、1 枚 1MB まで）。`attachment_outbox_root` を設定したときだけ使えます。

## 設定ファイル

```json
{
  "core_socket": "/path/to/opencrab/data/run/gate.sock",
  "data_dir": "/path/to/bluesky-gateway-data",
  "listen": "0.0.0.0:8795",
  "core_api": "http://127.0.0.1:8080",
  "attachment_outbox_root": "/path/to/opencrab/data/attachments/outbox",
  "oauth": {
    "client_id": "https://kojira.github.io/opencrab-bluesky-gateway/oauth/prod/client-metadata.json",
    "callback_url": "https://<ホスト>.<tailnet>.ts.net:8796/oauth/callback"
  }
}
```

| キー | 必須 | 内容 |
|---|---|---|
| `core_socket` | ○ | core の gate ソケット（core 設定の `gate.listen_socket`） |
| `data_dir` | ○ | bot の設定と受信の位置を置くフォルダ。`bots.json` はモード 0600 で書きます。 |
| `listen` |  | 設定画面の待ち受け。既定 `0.0.0.0:8795` |
| `core_api` |  | core の HTTP API。設定画面のエージェント一覧に使います。無ければエージェント ID を手で入れます。 |
| `attachment_outbox_root` |  | core の送信用ファイル置き場。画像の投稿に使います。 |
| `oauth` |  | 「Bluesky でログイン」を使うときの設定（下の「OAuth でログイン」）。無ければアプリパスワードだけです。 |

## 動かす

```sh
go build -o opencrab-bluesky-gateway ./cmd/opencrab-bluesky-gateway
./opencrab-bluesky-gateway config.json
```

### launchd で常駐させる

`deploy/com.kojira.opencrab-bluesky-gateway.plist` の `YOU` とパスを書き換えてから、次を実行します。

```sh
cp deploy/com.kojira.opencrab-bluesky-gateway.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.kojira.opencrab-bluesky-gateway.plist
```

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

OAuth では、Bluesky の認可サーバーがクライアントメタデータ（JSON）をインターネットから読みに来ます。戻り先（`callback_url`）はブラウザが開ければよいので、tailnet の中だけで構いません。

1. 戻り先を https にする（Tailscale）。設定画面が `8795` なら、たとえば `8796` で https にします。

   ```sh
   tailscale serve --bg --https=8796 http://127.0.0.1:8795
   ```

   `callback_url` は `https://<ホスト>.<tailnet>.ts.net:8796/oauth/callback` です。設定画面もこの https の URL で開けます。
2. 設定ファイルに `oauth`（`client_id` と `callback_url`）を書いて起動します。
3. `https://<ホスト>.<tailnet>.ts.net:8796/oauth/client-metadata.json` に出る JSON を、このリポジトリの `docs/oauth/<名前>/client-metadata.json` に置いて main に入れます。GitHub Pages（`docs/` を公開）で `https://kojira.github.io/opencrab-bluesky-gateway/oauth/<名前>/client-metadata.json` として公開され、これが `client_id` です。
   - 中身は公開鍵と戻り先だけです。秘密は入っていません。
   - `client_id` は、その JSON を取る URL と完全に同じでなければなりません。

公開しているメタデータ：

| 名前 | client_id |
|---|---|
| `qc` | https://kojira.github.io/opencrab-bluesky-gateway/oauth/qc/client-metadata.json |
| `prod` | https://kojira.github.io/opencrab-bluesky-gateway/oauth/prod/client-metadata.json |

## 開発

```sh
go vet ./...
go test -race ./...
```

## ライセンス

MIT
