# go-library-catalog

Owns the books the library holds.

```sh
PORT=8081 go run .
```

| Route | Behaviour |
|---|---|
| `GET /health` | `{"status":"ok"}` when the process is up |
| `GET /books` | JSON array of the catalogue, each book with `id`, `title`, `author` and `available` |
| `GET /books/{id}` | The single book with that `id`, or `404` when no book has it |

Run the tests with `go test ./...`.
