# ssm-local-script

SSM 接続のみ可能な EC2 インスタンスに対して、コマンドまたはローカルのスクリプトを実行し、標準出力・標準エラー出力を確認する Go 製 CLI です。

## 必要条件

- Go
- `fzf`
- AWS 認証情報 (`~/.aws/config`, `~/.aws/credentials` など)
- 実行対象インスタンスで SSM Agent が利用可能

## 使い方

### ビルド

```bash
go build -o ssm-local-script .
```

### 1. インラインコマンドを実行

```bash
./ssm-local-script -profile default -command 'uname -a'
```

```bash
./ssm-local-script -profile default -command 'ls -la /tmp/'
```

`-instance` を省略すると、EC2 一覧を `fzf` で選択します。

```bash
./ssm-local-script -profile default -instance i-0123456789abcdef0 -command 'hostname'
```

### 2. ローカルスクリプトを実行

```bash
./ssm-local-script -profile default -script ./scripts/deploy.sh -- --dry-run
```

指定したスクリプトはリモートの `/tmp` に転送され、実行されます。

`./ssm-local-script -command ./test.sh -- --dry-run` のように、`-command` にローカル実在ファイルを渡した場合はローカルスクリプトとして扱います。

## 主なオプション

- `-profile`: AWS プロファイル
- `-region`: AWS リージョン
- `-instance`: 対象インスタンス ID
- `-command`: リモートで実行するコマンド
- `-script`: ローカルスクリプトのパス
- `-workdir`: リモートでの作業ディレクトリ
- `-timeout`: 全体タイムアウト
- `-document`: 利用する SSM ドキュメント名（既定値: `AWS-RunShellScript`）

`-script` 使用時の追加引数は `--` 以降に指定します。
