// Command caddy is a Caddy build that includes the sql_executor handler,
// handy for local development. For production builds prefer xcaddy:
//
//	xcaddy build --with github.com/lorefnon/caddy-plugin-sql-executor
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	_ "github.com/caddyserver/caddy/v2/modules/standard"
	_ "github.com/lorefnon/caddy-plugin-sql-executor"
)

func main() {
	caddycmd.Main()
}
