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
| `reply` | `event`（e番号）、`text`、`files`（任意） | 返信。スレッドの root と parent を正しく付けます。 |
| `like` | `event` | いいね |
| `repost` | `event` | リポスト |
| `follow` | `user`（u番号、ハンドル、DID） | フォロー |

- 本文の URL と、`@ハンドル`（DID が引けるもの）は、リンクとして表示されます。
- `files` は、エージェントの作業フォルダの画像です（JPEG・PNG・WebP・GIF、4 枚まで、1 枚 1MB まで）。`attachment_outbox_root` を設定したときだけ使えます。

## 設定ファイル

```json
{
  "core_socket": "/path/to/opencrab/data/run/gate.sock",
  "data_dir": "/path/to/bluesky-gateway-data",
  "listen": "0.0.0.0:8795",
  "core_api": "http://127.0.0.1:8080",
  "attachment_outbox_root": "/path/to/opencrab/data/attachments/outbox"
}
```

| キー | 必須 | 内容 |
|---|---|---|
| `core_socket` | ○ | core の gate ソケット（core 設定の `gate.listen_socket`） |
| `data_dir` | ○ | bot の設定と受信の位置を置くフォルダ。`bots.json` はモード 0600 で書きます。 |
| `listen` |  | 設定画面の待ち受け。既定 `0.0.0.0:8795` |
| `core_api` |  | core の HTTP API。設定画面のエージェント一覧に使います。無ければエージェント ID を手で入れます。 |
| `attachment_outbox_root` |  | core の送信用ファイル置き場。画像の投稿に使います。 |

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

## 開発

```sh
go vet ./...
go test -race ./...
```

## ライセンス

MIT
