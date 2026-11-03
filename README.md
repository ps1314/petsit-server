# petsit-server

Backend for the「宠托排单助手」WeChat mini program: scheduling with conflict detection,
visit photo records and AI-written feeding reports. Deployed on WeChat Cloud Hosting from
this directory's `Dockerfile` (port 80).

```bash
go test ./...
HOST=127.0.0.1 DEV_OPENID=dev1 PORT=18081 go run .   # local, SQLite
```

Configuration is by environment variable; see `main.go`.
