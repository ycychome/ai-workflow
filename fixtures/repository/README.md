# Tiny Greeting Service

一个无需第三方依赖的 JSON 问候服务，支持默认问候和个性化姓名。需要 Python 3.11 或更新版本。

```sh
python3 app.py
curl http://127.0.0.1:8000/healthz
curl 'http://127.0.0.1:8000/api/greeting?name=Codex'
python3 -m unittest -v
```

- `GET /healthz` 返回 `{"status":"ok"}`。
- `GET /api/greeting` 返回 `greeting.txt` 中的默认问候。
- `GET /api/greeting?name=Codex` 返回 `{"message":"hello, Codex!"}`。
- 姓名支持中文和其他 Unicode 字符，去掉两端空格后长度为 1–80 个字符。非法参数返回 400，未知地址返回 404，POST 返回 405，问候配置不可用时返回 503。

默认监听 `127.0.0.1:8000`，可用 `--host`、`--port` 和 `--greeting-file` 指定配置。

M0 测试任务只将 `greeting.txt` 从 `hello` 改为 `hello M0`；预期 API 默认问候变成 `hello M0`，姓名问候变成 `hello M0, Codex!`。该文件更改会在下一次请求生效。
