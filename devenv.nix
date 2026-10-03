{
  pkgs,
  ...
}:

{
  # VeriDB is a Go project. The dev shell is intentionally small: Go toolchain,
  # a psql client for manual inspection, and the usual dev niceties.
  languages.go = {
    enable = true;
    package = pkgs.go_1_26;
  };

  packages = with pkgs; [
    # Local PostgreSQL server + client, so `veridb` can be exercised against a
    # throwaway database without touching a real host.
    postgresql_17
    # Go developer tooling.
    golangci-lint
    gotools
    gopls
    delve
    # Secret scanning. A sample credential was committed here once; the
    # pre-commit hook in .githooks uses this so it cannot happen again.
    gitleaks
    # Handy for querying / formatting SQL by hand.
    jq
    yq-go
  ];

  # Where veridb looks for its config by default. Override with
  # `VERIDB_CONFIG=/path/to/other.yaml` in your environment when needed.
  env = {
    VERIDB_CONFIG = "configs/veridb.yaml";
  };

  # Note: devenv's dotenv integration is deliberately NOT enabled.
  #
  # It would export .env into the shell, and devenv then writes the whole
  # environment, credentials included, into .devenv/shell-*.sh at mode 0755.
  # That turns a 0600 .env into a world-readable copy of every password.
  #
  # VeriDB reads .env itself at startup (godotenv, and -env-file for an explicit
  # path), and `docker compose` reads .env natively, so nothing here needs the
  # values in the shell environment. When you do want them for a manual command:
  #
  #   set -a; . ./.env; set +a
  #
  # Keep .env at mode 0600.

  scripts.test.exec = "go test ./...";
  scripts.build.exec = "go build ./cmd/veridb";
  scripts.secrets.exec = "gitleaks git --no-banner --redact";

  enterShell = ''
    # Point git at the repository's hooks so secret scanning is automatic for
    # anyone using this shell, rather than something to remember.
    if git rev-parse --git-dir >/dev/null 2>&1; then
      if [ "$(git config --get core.hooksPath 2>/dev/null)" != ".githooks" ]; then
        git config core.hooksPath .githooks
        echo "veridb: enabled git hooks from .githooks (secret scanning on commit)"
      fi
    fi

    echo "veridb devenv | go $(go version | cut -d' ' -f3) | psql $(psql --version | cut -d' ' -f3)"
  '';
}
