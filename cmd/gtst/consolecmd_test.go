// Copyright 2016 The go-ethereum Authors
// This file is part of go-tstereum.
//
// go-tstereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-tstereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-tstereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"crypto/rand"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tnechain/go-tnechain/params"
)

const (
	ipcAPIs  = "admin:1.0 debug:1.0 tst:1.0 miner:1.0 net:1.0 personal:1.0 rpc:1.0 shh:1.0 txpool:1.0 web3:1.0"
	httpAPIs = "tst:1.0 net:1.0 rpc:1.0 web3:1.0"
)

// Tests that a node embedded within a console can be started up properly and
// then terminated by closing the input stream.
func TestConsoleWelcome(t *testing.T) {
	coinbase := "0x8605cdbbdb6d264aa742e77020dcbc58fcdce182"

	// Start a gtst console, make sure it's cleaned up and terminate the console
	gtst := runGtst(t,
		"--port", "0", "--maxpeers", "0", "--nodiscover", "--nat", "none",
		"--tsterbase", coinbase, "--shh",
		"console")

	// Gather all the infos the welcome message needs to contain
	gtst.SetTemplateFunc("goos", func() string { return runtime.GOOS })
	gtst.SetTemplateFunc("goarch", func() string { return runtime.GOARCH })
	gtst.SetTemplateFunc("gover", runtime.Version)
	gtst.SetTemplateFunc("gtstver", func() string { return params.Version })
	gtst.SetTemplateFunc("niltime", func() string { return time.Unix(0, 0).Format(time.RFC1123) })
	gtst.SetTemplateFunc("apis", func() string { return ipcAPIs })

	// Verify the actual welcome message to the required template
	gtst.Expect(`
Welcome to the Gtst JavaScript console!

instance: Gtst/v{{gtstver}}/{{goos}}-{{goarch}}/{{gover}}
coinbase: {{.Tsterbase}}
at block: 0 ({{niltime}})
 datadir: {{.Datadir}}
 modules: {{apis}}

> {{.InputLine "exit"}}
`)
	gtst.ExpectExit()
}

// Tests that a console can be attached to a running node via various means.
func TestIPCAttachWelcome(t *testing.T) {
	// Configure the instance for IPC attachement
	coinbase := "0x8605cdbbdb6d264aa742e77020dcbc58fcdce182"
	var ipc string
	if runtime.GOOS == "windows" {
		ipc = `\\.\pipe\gtst` + strconv.Itoa(trulyRandInt(100000, 999999))
	} else {
		ws := tmpdir(t)
		defer os.RemoveAll(ws)
		ipc = filepath.Join(ws, "gtst.ipc")
	}
	// Note: we need --shh because testAttachWelcome checks for default
	// list of ipc modules and shh is included there.
	gtst := runGtst(t,
		"--port", "0", "--maxpeers", "0", "--nodiscover", "--nat", "none",
		"--tsterbase", coinbase, "--shh", "--ipcpath", ipc)

	time.Sleep(2 * time.Second) // Simple way to wait for the RPC endpoint to open
	testAttachWelcome(t, gtst, "ipc:"+ipc, ipcAPIs)

	gtst.Interrupt()
	gtst.ExpectExit()
}

func TestHTTPAttachWelcome(t *testing.T) {
	coinbase := "0x8605cdbbdb6d264aa742e77020dcbc58fcdce182"
	port := strconv.Itoa(trulyRandInt(1024, 65536)) // Yeah, sometimes this will fail, sorry :P
	gtst := runGtst(t,
		"--port", "0", "--maxpeers", "0", "--nodiscover", "--nat", "none",
		"--tsterbase", coinbase, "--rpc", "--rpcport", port)

	time.Sleep(2 * time.Second) // Simple way to wait for the RPC endpoint to open
	testAttachWelcome(t, gtst, "http://localhost:"+port, httpAPIs)

	gtst.Interrupt()
	gtst.ExpectExit()
}

func TestWSAttachWelcome(t *testing.T) {
	coinbase := "0x8605cdbbdb6d264aa742e77020dcbc58fcdce182"
	port := strconv.Itoa(trulyRandInt(1024, 65536)) // Yeah, sometimes this will fail, sorry :P

	gtst := runGtst(t,
		"--port", "0", "--maxpeers", "0", "--nodiscover", "--nat", "none",
		"--tsterbase", coinbase, "--ws", "--wsport", port)

	time.Sleep(2 * time.Second) // Simple way to wait for the RPC endpoint to open
	testAttachWelcome(t, gtst, "ws://localhost:"+port, httpAPIs)

	gtst.Interrupt()
	gtst.ExpectExit()
}

func testAttachWelcome(t *testing.T, gtst *testgtst, endpoint, apis string) {
	// Attach to a running gtst note and terminate immediately
	attach := runGtst(t, "attach", endpoint)
	defer attach.ExpectExit()
	attach.CloseStdin()

	// Gather all the infos the welcome message needs to contain
	attach.SetTemplateFunc("goos", func() string { return runtime.GOOS })
	attach.SetTemplateFunc("goarch", func() string { return runtime.GOARCH })
	attach.SetTemplateFunc("gover", runtime.Version)
	attach.SetTemplateFunc("gtstver", func() string { return params.Version })
	attach.SetTemplateFunc("tsterbase", func() string { return gtst.Tsterbase })
	attach.SetTemplateFunc("niltime", func() string { return time.Unix(0, 0).Format(time.RFC1123) })
	attach.SetTemplateFunc("ipc", func() bool { return strings.HasPrefix(endpoint, "ipc") })
	attach.SetTemplateFunc("datadir", func() string { return gtst.Datadir })
	attach.SetTemplateFunc("apis", func() string { return apis })

	// Verify the actual welcome message to the required template
	attach.Expect(`
Welcome to the Gtst JavaScript console!

instance: Gtst/v{{gtstver}}/{{goos}}-{{goarch}}/{{gover}}
coinbase: {{tsterbase}}
at block: 0 ({{niltime}}){{if ipc}}
 datadir: {{datadir}}{{end}}
 modules: {{apis}}

> {{.InputLine "exit" }}
`)
	attach.ExpectExit()
}

// trulyRandInt generates a crypto random integer used by the console tests to
// not clash network ports with other tests running cocurrently.
func trulyRandInt(lo, hi int) int {
	num, _ := rand.Int(rand.Reader, big.NewInt(int64(hi-lo)))
	return int(num.Int64()) + lo
}
