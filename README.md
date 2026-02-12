# play-bin

Dockerコンテナとして稼働するゲームサーバー等を、Web UIやDiscordから一括管理するためのツールです。

## config.jsonについて

`config.example.json`を`config.json`という名前でコピーし、環境に合わせて各項目を編集します。

- `httpListen?: string` - Web UIを待機するアドレスとポート (省略時は無効)
- `sftpListen?: string` - SFTPサーバーを待機するアドレスとポート (省略時は無効)
- `users: map<username: string, UserConfig>` - ユーザー設定
  - `discord?: string` - ユーザーのDiscord ID
  - `password: string` - Web UIおよびSFTPログインに使用するパスワード
  - `permissions: map<servername string, ("read" | "write" | "execute")[]>` - 操作権限の設定
    `servername`に`*`を指定するとすべてのサーバーに対して権限を設定
- `servers: map<servername string, ServerConfig>` - サーバー設定
  - `workingDir?: string` - 作業ディレクトリ
  - `compose?: Object` - コンテナ定義
    - `image: string` - Dockerイメージ
    - `command?: Object` - コンテナ起動コマンド
      - `entrypoint?: string` - エントリーポイント
      - `arguments?: string` - コマンド引数
    - `restart?: "always"|"no"|"on-failure"|"unless-stopped"` - 再起動ポリシー
      - `no`: コンテナが停止しても再起動しない(初期値)
      - `always`: 必ず再起動する
      - `on-failure`: コンテナが異常終了した場合に再起動する
      - `unless-stopped`: コンテナが停止しても再起動する
    - `network?: Object` - ネットワーク設定
      - `mode: "host"|"bridge"` - ネットワークモード
        - `host`: ホストネットワーク
        - `bridge`: ブリッジネットワーク
      - `mapping?: map<string, string>` - ポートマッピング
    - `mount?: map<string, string>` - マウント設定 (ホストパス: コンテナパス)
  - `commands: Object` - コマンド定義
    - `stop?: CmdConfig[]` - 停止時に実行するコマンドリスト
    - `backup?: CmdConfig[]` - バックアップ時に実行するコマンドリスト
      - `type: "attach" | "exec" | "log" | "sleep" | "backup"` - コマンド種別
        - `attach`: コンテナに接続
        - `exec`: コンテナ内でコマンドを実行
        - `log`: コンテナログを取得
        - `sleep`: 指定時間待機
        - `backup`: バックアップ
      - `arg: string` - コマンド引数 (backup種別の場合は `src:destBase` 形式)
    - `message?: string` - Discord通知メッセージのフォーマット
  - `discord?: Object` - Discord設定
    - `token?: string` - Discord Botトークン (channelとセット)
    - `channel?: string` - DiscordチャンネルID (tokenとセット)
    - `webhook?: string` - Discord Webhook URL (logSettingとセット)
    - `logSetting?: string` - ログ設定ファイルのパス (webhookとセット)

```json
{
  "httpListen": ":8080", //
  "sftpListen": ":2022",
  "users": {
    "admin": {
      "discord": "123456789012345678",
      "password": "strongpassword",
      "permissions": {
        "*": ["read", "write", "execute"]
      }
    }
  },
  "servers": {
    "minecraft-1": {
      "workingDir": "/home/atomu/minecraft",
      "compose": {
        "image": "mc_java:21",
        "command": {
          "entrypoint": "java",
          "arguments": "-Xms2G -Xmx2G -jar server.jar nogui"
        },
        "restart": "always",
        "network": {
          "mode": "bridge",
          "mapping": {
            "25565": "25565"
          }
        },
        "mount": {
          "/home/atomu/minecraft/data": "/MC"
        }
      },
      "commands": {
        "stop": [
          {
            "type": "attach",
            "arg": "stop\n"
          },
          {
            "type": "sleep",
            "arg": "10s"
          }
        ],
        "backup": [
          {
            "type": "attach",
            "arg": "save-all"
          },
          {
            "type": "sleep",
            "arg": "5s"
          },
          {
            "type": "backup",
            "arg": "/home/atomu/minecraft/data:/home/atomu/backups/minecraft-1"
          }
        ],
        "message": "[Discord] ${user}: ${message}"
      },
      "discord": {
        "token": "YOUR_BOT_TOKEN",
        "channel": "123456789012345678",
        "webhook": "https://discord.com/api/webhooks/...",
        "logSetting": "./logs.json"
      }
    }
  }
}
```

### 主な機能

- Webブラウザからのコンテナ操作（起動・停止・コンソール表示）
- Discordスラッシュコマンドによるコンテナ制御
- コンテナログの特定キーワードを検知してDiscordへ通知
- SFTPサーバー機能による、安全で高速なファイル管理
- rsyncを利用した効率的なインクリメンタルバックアップ

### 導入手順

1. 実行バイナリの準備
   Go言語がインストールされた環境で `go build .` を実行し、実行ファイルを生成します。

2. 設定ファイルの作成
   `config.example.json` を `config.json` という名前でコピーし、環境に合わせて各項目を編集します。詳細は後述の「設定ファイルの解説」を参照してください。

3. ログ通知設定の作成（任意）
   コンテナログをDiscordに転送したい場合は、`logs.json` を開き、正規表現と転送先のWebhook URLを設定します。

4. サービスの起動
   生成されたバイナリを実行します。

### 設定ファイルの解説

設定ファイル（config.json）の主要なブロックについて説明します。

### 全般設定 (top level)

- `httpListen`: Web UIおよびAPIサーバーが待機するアドレスとポートです。省略または空欄にするとWebサーバーは起動しません。
- `sftpListen`: ファイル操作用SFTPサーバーが待機するアドレスとポートです。省略または空欄にするとSFTPサーバーは起動しません。

### ユーザー設定 (users)

管理権限を持つユーザーを定義します。

- `discord`: ユーザーのDiscord IDです。
- `password`: Web UIおよびSFTPログインに使用するパスワードです。
- `permissions`: 操作権限の設定です。サーバー名（または `*`）をキーとし、以下の権限リストを指定します。
  - `read`: 状態閲覧、ログ閲覧、統計情報、ファイル一覧
  - `write`: コンソール操作、ファイル書き込み・削除
  - `execute`: コンテナの起動・停止・再起動・バックアップ

### サーバー設定 (servers)

管理対象となるコンテナ（サーバー）ごとに設定を行います。

- `workingDir`: ホスト側での作業ディレクトリです。
- `image`: 使用するDockerイメージ名です。
- `network`: ポート開放やネットワークモードの設定です。
- `mount`: ホストのディレクトリとコンテナ内のパスを紐付けます（SFTPはこの設定を元にアクセス範囲を決定します）。
- `commands.backup`: バックアップの元パス（src）と保存先（dest）を指定します。
- `discord.token`: Discord Bot用のトークンですが、省略可能です（省略した場合はそのサーバーのBot機能が無効になります）。
- `discord.channel`: 操作コマンドを受け取るチャンネルIDですが、省略可能です。

### SFTPサーバーの利用方法

SFTP機能を使うことで、WinSCPやFileZillaなどのツールから直接コンテナ内のファイルを編集できます。

1. SFTPクライアントソフトを開きます。
2. ホスト名にサーバーのIPアドレス、ポートに設定した番号（例: 2022）を入力します。
3. `config.json` に設定したユーザー名とパスワードでログインします。
4. ログインに成功すると、許可されたコンテナの名前がディレクトリとして表示されます。

### バックアップの実行

1. Web UIまたはDiscordから「backup」アクションを実行します。
2. 内部でコンテナを安全に停止させた後、rsyncによる差分バックアップが行われます。
3. バックアップはタイムスタンプが付与されたフォルダに保存され、最新版は `latest` という名前でリンクされます。
