---
name: drift-fixture
description: A skill with one of each defect the drift test must catch, beside the forms it must let through.
metadata:
  version: dev
---

# Fixture

Valid, and never reported:

```sh
tracepad traces ls --since 1h --env production | jq '.traces'
T=$(tracepad traces last --error --json) && echo "$T"
tracepad skills install --dir /tmp/skills --force
tracepad scores add --trace <trace-id> --name helpful \
  --value 1
tracepad stats --group-by total   # a comment is not a --flag
tracepad skills show debugging.md
pip install tracepad && npm install tracepad @opentelemetry/api
docker run --rm --user "$(id -u):$(id -g)" -v "$HOME/.claude/skills:/skills" \
  ghcr.io/tracepad/tracepad skills install --dir /skills
TRACEPAD_PROJECTS="app:tp-pk-1:tp-sk-1" nohup tracepad serve --listen localhost:4318 --data-dir /tmp/x >> "/tmp/x/log" 2>&1 &
tracepad traces ls --limit 5 \
```

```sh
echo "a block's trailing backslash does not reach this one" --bogus
```

`get_trace`, `search`, `ttft_ms`, `total_cost`, `GET /api/v1`,
`/api/v1/traces/{id}`, `DELETE /api/v1/traces/<trace-id>`,
`TRACEPAD_API_KEY="$TOKEN" tracepad projects ls`,
[a reference](references/inline.md),
[the SDK](https://github.com/tracepad/tracepad/blob/main/docs/sdk-python.md#evals).

Defects, one each:

```sh
tracepad tracez ls
tracepad traces lst
tracepad traces ls --sinse 1h
tracepad skills install --global
tracepad serve --port 9999
tracepad serve --listen localhost:9999 extra
tracepad mcp --url http://localhost:9999
tracepad skills show debuging.md
$ tracepad traces lsx
sudo ./bin/tracepad trace ls
docker run --rm ghcr.io/tracepad/tracepad:0.4.0 skills instal --dir /skills
```

`get_trcae` is not a tool, `/api/v1/tracez` is not a route, and
`DELETE /api/v1/system` is not a method it serves.

[CLI docs](../../../docs/cli.md), [nothing](references/nowhere.md),
[no file](https://github.com/tracepad/tracepad/blob/main/docs/nowhere.md),
[no heading](https://github.com/tracepad/tracepad/blob/main/docs/cli.md#no-such-heading).
