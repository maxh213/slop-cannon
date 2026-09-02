# fixture

A tiny Go command-line tool that prints statistics for a list of numbers.
It exists as a test fixture for slop-cannon: small enough to read in a
minute, with a real test suite an agent can run.

## Usage

```sh
go run . 3 1 4 1 5
```

prints the sum, mean, and maximum of the arguments.

## Development

```sh
make build   # go build ./...
make test    # go test ./...
```

## Status

- `Sum`, `Mean`, and `Max` are implemented in `stats/`.
- `Median` is planned but not implemented yet.
