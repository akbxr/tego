package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/akbxr/tego/internal/ext"
	"github.com/akbxr/tego/internal/mcp"
)

// cmdTrust is `tego trust`: it trusts the project's MCP servers and
// extensions in the current folder as their files are now, without a
// session, so tego -p and CI, which can't ask, run them too. /mcp trust and
// /extensions trust do the same in a session.
func cmdTrust(home string, args []string) error {
	if len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			fmt.Println("usage: tego trust\n\nTrusts the MCP servers in this project's .mcp.json and .tego/mcp.json and the extensions in its .tego/extensions,\nas their files are now: they run in every tego here (tego -p too) until a file changes. Its worktrees are covered.")
			return nil
		}
		return fmt.Errorf("unknown argument %q (usage: tego trust)", args[0])
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	servers, mErr := mcp.TrustProject(home, cwd)
	exts, eErr := ext.TrustProject(home, cwd)
	if len(servers) > 0 {
		fmt.Printf("trusted %s: %s\n", plural(len(servers), "MCP server"), strings.Join(servers, ", "))
	}
	if len(exts) > 0 {
		fmt.Printf("trusted %s: %s\n", plural(len(exts), "extension"), strings.Join(exts, ", "))
	}
	if err := errors.Join(mErr, eErr); err != nil {
		return err
	}
	if len(servers)+len(exts) == 0 {
		fmt.Println("this project has no MCP servers or extensions of its own")
		return nil
	}
	fmt.Println("they run until their files change; tego trust again after reading the change")
	return nil
}
