# システム設計書: Play-Bin

## A. 技術スタック (Tech Stack)

### Languages & Frameworks

- **Go (1.25.5)**: バックエンドの主要言語
- **Discordgo**: Discord Bot API クライアント
- **Docker SDK for Go**: Docker エンジン操作
- **Gorilla WebSocket**: リアルタイム通信 (ターミナル/ステータス)
- **pkg/sftp & crypto/ssh**: SFTP/SSH サーバー実装

### Infrastructure & Runtime

- **Docker Engine**: コンテナランタイム
- **Local Storage**: JSON設定ファイル (`config.json`) / SFTP ホストキー
- **Rsync**: バックアップ/リストア処理用 (CLI 実行)

### Development Tools

- **VSCode**: 推奨開発環境
- **Gemini / Antigravity**: AI 支援

## B. システム概要 (High-Level Overview)

Play-Bin は、Docker コンテナとして実行されるゲームサーバー群を統合管理するためのミドルウェアです。
ユーザーは Web UI (HTTP/WebSocket) や Discord Bot、SFTP クライアントを通じて、コンテナの操作（起動・停止）、ファイル管理、コマンド送信、バックアップ・リストアを行うことができます。
設定は単一の `config.json` で管理され、ホットリロードに対応しているため、動的なサーバー構成変更が可能です。
各インターフェース（API, Discord, SFTP）は権限管理システムにより保護されており、ユーザーごとにアクセス可能なサーバーや操作を制限します。

## C. アーキテクチャ図 (Architecture Diagram)

```mermaid
graph TD
    subgraph "Frontend / Client Layer"
        WebClient[("Web Browser")]
        DiscordClient[("Discord Client")]
        SFTPClient[("SFTP Client")]
    end

    subgraph "Backend / API Layer"
        Main[("main.go")]
        ConfigPkg[("internal/config")]
        LoggerPkg[("internal/logger")]

        APIServer[("internal/api/server.go")]
        AuthMiddleware[("internal/api/auth.go")]
        ContainerHandlers[("internal/api/handlers_containers.go")]
        WSHandlers[("internal/api/handlers_ws.go")]

        DiscordManager[("internal/discord/bot.go")]

        SFTPServer[("internal/sftp/server.go")]
        VFSHandler[("internal/sftp/vfs.go")]

        ContainerManager[("internal/container/container.go")]
        DockerClientWrapper[("internal/docker/docker.go")]
    end

    subgraph "Infrastructure / Data Layer"
        ConfigFile[("(Local) config.json")]
        HostKey[("(Local) sftp_host_key")]
        DockerEngine[("Docker Engine")]
        LocalVolumes[("(Local) Server Data")]
    end

    subgraph "External Services"
        DiscordAPI[("Discord API")]
    end

    %% Connections
    WebClient -- "HTTP / WebSocket" --> APIServer
    DiscordClient -- "Interaction / Message" --> DiscordAPI
    SFTPClient -- "SSH / SFTP" --> SFTPServer

    DiscordAPI -- "Gateway / Webhook" --> DiscordManager

    Main --> ConfigPkg
    Main --> DockerClientWrapper
    Main --> APIServer
    Main --> DiscordManager
    Main --> SFTPServer

    APIServer --> AuthMiddleware
    AuthMiddleware --> ConfigPkg
    APIServer --> ContainerHandlers
    APIServer --> WSHandlers
    ContainerHandlers --> ContainerManager
    WSHandlers --> DockerClientWrapper

    DiscordManager --> ConfigPkg
    DiscordManager --> ContainerManager
    DiscordManager --> DockerClientWrapper

    SFTPServer --> ConfigPkg
    SFTPServer --> VFSHandler
    VFSHandler --> LocalVolumes

    ContainerManager --> ConfigPkg
    ContainerManager --> DockerClientWrapper
    ContainerManager --> LocalVolumes

    DockerClientWrapper -- "Docker Socket (Unix/TCP)" --> DockerEngine
    ConfigPkg -- "Read / Watch" --> ConfigFile
    SFTPServer -- "Read" --> HostKey
```

## D. コンポーネント詳細

### Frontend / Client Layer

- **Web Browser**: シングルページアプリケーション (SPA) などを通じてシステムのGUIを利用。
- **Discord Client**: チャットコマンドやGUIボタンでサーバーを操作。
- **SFTP Client**: ファイル転送ソフト (WinSCP, FileZilla等) でサーバーファイルを直接編集。

### Backend / API Layer

- **main.go**: アプリケーションのエントリーポイント。各サービスの初期化と起動順序を制御。
- **internal/api**:
  - **Server**: HTTPサーバーとルーティング定義。
  - **Auth**: トークンベースの認証と権限チェックを行うミドルウェア。
  - **Container Handlers**: コンテナ一覧、操作、ログ取得などのREST API実装。
  - **WS Handlers**: xterm.js 用のターミナル通信やリアルタイムステータス配信。
- **internal/discord**:
  - **Bot Manager**: 複数トークンに対応したBotセッション管理。
  - **Interactions**: スラッシュコマンド (`/action`, `/cmd`) の処理と権限確認。
- **internal/sftp**:
  - **Server**: SSHプロトコルを用いたSFTPサーバー。
  - **VFS Handler**: 仮想ファイルシステム。Dockerマウントポイントをユーザーごとのルートディレクトリにマッピング。
- **internal/container**:
  - **Manager**: コンテナのライフサイクル管理 (Start, Stop, Kill) および rsync を用いたバックアップ/リストア機能。
- **internal/config**:
  - JSON設定ファイルの読み込み、構造化、およびホットリロード機能の提供。
- **internal/docker**:
  - Docker SDK のラッパー。初期化や共通エラー処理を担当。

### Infrastructure / Data Layer

- **(Local) config.json**: ユーザー、サーバー、権限、コマンド定義を含む設定ファイル。
- **(Local) sftp_host_key**: SSHサーバーのホスト認証用秘密鍵。
- **Docker Engine**: 実際のコンテナ実行を担うデーモン。
- **(Local) Server Data**: コンテナにバインドマウントされるホスト上の実データディレクトリ。

### External Services

- **Discord API**: Bot の接続、メッセージ送受信、インタラクションイベントの提供。

## E. データ構成

```mermaid
graph LR
    Root[("Project Root")]

    subgraph "Configuration & Assets"
        Config["config.json"]
        CfgEg["config.example.json"]
        HostKey["sftp_host_key"]
        HostKeyPub["sftp_host_key.pub"]
        WebIndex["index.html"]
    end

    subgraph "Source Code"
        MainGo["main.go"]

        subgraph "Internal Packages"
            API["internal/api/"]
            ConfigPkg["internal/config/"]
            Container["internal/container/"]
            Discord["internal/discord/"]
            Docker["internal/docker/"]
            Logger["internal/logger/"]
            SFTP["internal/sftp/"]
        end
    end

    Root --> Config
    Root --> CfgEg
    Root --> HostKey
    Root --> HostKeyPub
    Root --> WebIndex
    Root --> MainGo
    Root --> Internal Packages

    MainGo -.-> API
    MainGo -.-> ConfigPkg
    MainGo -.-> Container
    MainGo -.-> Discord
    MainGo -.-> Docker
    MainGo -.-> Logger
    MainGo -.-> SFTP
```
