# 🛰️ hostprobe

One-shot Linux CLI that reads host records from PostgreSQL, checks each one with ICMP ping or SSH,
writes `status` and `last_check` back, prints the results, and exits. Ships as a single static binary.

## ✨ Features

- 🏓 **Ping check**: one echo request with a hard deadline; IPv4 and IPv6.
- 🔐 **SSH check**: `online` only after non-interactive authentication with strict host keys; offline
  reasons are categorized (timeout, refused, unreachable, authentication, host key).
- ⚡ **Concurrent**: independent limits and timeouts for ping and SSH.
- 🗄️ **Schema-safe**: validates `public.hosts`, never creates or alters it, updates only `status` and
  `last_check`.
- 🔤 **Status formats**: text `online`/`offline`, text `true`/`false`, or native boolean, detected
  automatically.
- ⏭️ **Invalid rows**: reported, skipped, and left unchanged.
- 🔁 **Failover**: up to three backup databases, tried in order when the primary is unreachable.
- 📝 **First run**: creates a user-only config template.
- 🌈 **Output**: colored with emojis; plain when redirected or `NO_COLOR` is set.

## 📦 Install

Building needs Go 1.26+. Running needs Linux `ping` (`iputils`), the OpenSSH client, and PostgreSQL access.

```bash
git clone https://github.com/tf4482/hostprobe.git
cd hostprobe
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/hostprobe .
sudo install -m 755 dist/hostprobe /usr/local/bin/hostprobe
```

## 🗄️ Database

`public.hosts` must exist. The role needs `CONNECT`, schema `USAGE`, table `SELECT`, and
`UPDATE (status, last_check)`.

| Column | Use |
| --- | --- |
| `id` | Non-null, unique identifier |
| `name` | Display name |
| `status` | Text `online`/`offline`, text `true`/`false`, or boolean |
| `last_check` | Timestamp of the latest saved check |
| `method` | `ping` or `ssh` (whitespace and case are ignored) |
| `ip` | IPv4 or IPv6 address |
| `ssh_user`, `ssh_port` | Required only when SSH rows exist; port `1..65535` |

Text statuses must use one vocabulary consistently; null, blank, or mixed values stop the run.

## ⚙️ Configuration

The first existing file is used, without merging:

1. `config.yml` beside the executable
2. `~/.config/hostprobe/config.yml`

If neither exists, the second one is created with dummy values and the run exits `1`. See
[`config.example.yml`](config.example.yml):

| Setting | Description |
| --- | --- |
| `database.host`, `port`, `name`, `user`, `password` | Primary PostgreSQL target |
| `database.backups` | Optional list of up to three failover targets with the same keys |
| `settings.ping_concurrency`, `ssh_concurrency` | Parallel checks per method |
| `settings.ping_timeout_seconds`, `ssh_timeout_seconds` | Hard deadline per check |

## 🚀 Usage

```bash
hostprobe
hostprobe --help   # also: -h, help
```

SSH checks use the SSH agent or configured keys; populate `known_hosts` first.

| Exit | Meaning |
| --- | --- |
| `0` | All valid records checked and saved; offline hosts are not errors |
| `1` | Configuration, validation, dependency, database, skipped-row, or save error |
| `130` | Interrupted by the user |

## ⏱️ Deploy

Install the binary, place the configuration, then schedule it with the example systemd units. The timer
waits for the previous run to finish, so runs never overlap.

```bash
sudo install -m 644 hostprobe.example.service /etc/systemd/system/hostprobe.service
sudo install -m 644 hostprobe.example.timer /etc/systemd/system/hostprobe.timer
sudo systemctl daemon-reload
sudo systemctl enable --now hostprobe.timer
journalctl -u hostprobe.service
```

Adjust `User=` in the service; that account needs the configuration and its SSH keys.

## 🧪 Develop

```bash
go vet ./...
go test ./...
```

## 📜 License

MIT, see [LICENSE](LICENSE).
