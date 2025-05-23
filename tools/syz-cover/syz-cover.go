// Copyright 2018 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

// syz-cover generates coverage HTML report from raw coverage files.
// Raw coverage files are text files with one PC in hex form per line, e.g.:
//
//	0xffffffff8398658d
//	0xffffffff839862fc
//	0xffffffff8398633f
//
// Raw coverage files can be obtained either from /rawcover manager HTTP handler,
// or from syz-execprog with -coverfile flag.
//
// Usage:
//
//	syz-cover -config config_file rawcover.file*
//
// or use all pcs in rg.Symbols
//
//	syz-cover -config config_file
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/civil"
	"github.com/google/syzkaller/pkg/cover"
	"github.com/google/syzkaller/pkg/cover/backend"
	"github.com/google/syzkaller/pkg/coveragedb"
	"github.com/google/syzkaller/pkg/covermerger"
	"github.com/google/syzkaller/pkg/log"
	"github.com/google/syzkaller/pkg/mgrconfig"
	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/pkg/tool"
	"github.com/google/syzkaller/pkg/vminfo"
)

var (
	flagConfig  = flag.String("config", "", "configuration file")
	flagModules = flag.String("modules", "",
		"modules JSON info obtained from /modules (optional)")
	flagPeriod = flag.String("period", "day", "time period(day[default], month, quarter)")
	flagDateTo = flag.String("to",
		civil.DateOf(time.Now()).String(), "heatmap date to(optional)")
	flagForFile = flag.String("for-file", "", "[optional]show file coverage")
	flagRepo    = flag.String("repo", "git://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git",
		"[optional] repo to be used by -for-file")
	flagCommit       = flag.String("commit", "latest", "[optional] commit to be used by -for-file")
	flagNamespace    = flag.String("namespace", "upstream", "[optional] used by -for-file")
	flagDebug        = flag.Bool("debug", false, "[optional] enables detailed output")
	flagSourceCommit = flag.String("source-commit", "", "[optional] filter input commit")
	flagExports      = flag.String("exports", "cover",
		"[optional] comma separated list of exports for which we want to generate coverage, "+
			"possible values are: cover, subsystem, module, funccover, json, jsonl, rawcover, rawcoverfiles, all")
	flagForce = flag.Bool("force", false, "[optional] create coverage report when "+
		"there are missing coverage callbacks")
)

func toolFileCover() {
	dateTo, err := civil.ParseDate(*flagDateTo)
	if err != nil {
		tool.Failf("failed to parse date from: %v", err)
	}
	tp, err := coveragedb.MakeTimePeriod(dateTo, *flagPeriod)
	if err != nil {
		tool.Fail(err)
	}
	config := cover.DefaultTextRenderConfig()
	config.ShowLineSourceExplanation = *flagDebug
	mr, err := cover.GetMergeResult(context.Background(),
		*flagNamespace,
		*flagRepo,
		*flagCommit,
		*flagSourceCommit,
		*flagForFile,
		nil, tp)
	if err != nil {
		tool.Fail(err)
	}

	details, err := cover.RendFileCoverage(
		*flagRepo,
		*flagCommit,
		*flagForFile,
		covermerger.MakeWebGit(nil), // get files directly from WebGits
		mr,
		config,
	)
	if err != nil {
		tool.Fail(err)
	}
	fmt.Println(details)
}

func initModules(cfg *mgrconfig.Config) []*vminfo.KernelModule {
	modules, err := backend.DiscoverModules(cfg.SysTarget, cfg.KernelObj, cfg.ModuleObj)
	if err != nil {
		tool.Fail(err)
	}
	if *flagModules != "" {
		m, err := loadModules(*flagModules)
		if err != nil {
			tool.Fail(err)
		}
		modules = m
	}
	return modules
}

func main() {
	defer tool.Init()()
	if *flagForFile != "" {
		toolFileCover()
		return
	}
	cfg, err := mgrconfig.LoadFile(*flagConfig)
	if err != nil {
		tool.Fail(err)
	}
	modules := initModules(cfg)
	rg, err := cover.MakeReportGenerator(cfg, modules)
	if err != nil {
		tool.Fail(err)
	}
	pcs := initPCs(rg)
	progs := []cover.Prog{{PCs: pcs}}
	params := cover.HandlerParams{
		Progs: progs,
		Debug: *flagDebug,
		Force: *flagForce,
	}

	if *flagExports == "all" {
		*flagExports = "cover,subsystem,module,funccover,rawcover,rawcoverfiles"
	}
	exports := strings.Split(*flagExports, ",")
	for _, export := range exports {
		log.Logf(1, "start generate %v", export)
		switch export {
		case "cover":
			doReport(params, "syz-cover.html", rg.DoHTML)
		case "subsystem":
			doReport(params, "syz-cover-subsystem.html", rg.DoSubsystemCover)
		case "module":
			doReport(params, "syz-cover-module.html", rg.DoModuleCover)
		case "funccover":
			doReport(params, "syz-cover-funccover.csv", rg.DoFuncCover)
		case "rawcover":
			doReport(params, "rawcoverpcs", rg.DoRawCover)
		case "rawcoverfiles":
			doReport(params, "rawcoverfiles", rg.DoRawCoverFiles)
		case "json":
			doReport(params, "json", rg.DoLineJSON)
		case "jsonl":
			doReport(params, "jsonl", rg.DoCoverJSONL)
		default:
			tool.Failf("unknown export type: %q", export)
		}
	}
}

func doReport(params cover.HandlerParams, fname string,
	fn func(w io.Writer, params cover.HandlerParams) error) {
	buf := new(bytes.Buffer)
	if err := fn(buf, params); err != nil {
		tool.Fail(err)
	}
	log.Logf(0, "write to %v", fname)
	if err := osutil.WriteFile(fname, buf.Bytes()); err != nil {
		tool.Fail(err)
	}
	exec.Command("xdg-open", fname).Start()
}

func initPCs(rg *cover.ReportGenerator) []uint64 {
	var pcs []uint64
	if len(flag.Args()) == 0 {
		pcs = rg.CallbackPoints
		return pcs
	}
	pcs, err := readPCs(flag.Args())
	if err != nil {
		tool.Fail(err)
	}
	return pcs
}

func readPCs(files []string) ([]uint64, error) {
	var pcs []uint64
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		for s := bufio.NewScanner(bytes.NewReader(data)); s.Scan(); {
			line := strings.TrimSpace(s.Text())
			if line == "" {
				continue
			}
			pc, err := strconv.ParseUint(line, 0, 64)
			if err != nil {
				return nil, err
			}
			pcs = append(pcs, pc)
		}
	}
	return pcs, nil
}

func loadModules(fname string) ([]*vminfo.KernelModule, error) {
	data, err := os.ReadFile(fname)
	if err != nil {
		return nil, err
	}
	var modules []*vminfo.KernelModule
	err = json.Unmarshal(data, &modules)
	if err != nil {
		return nil, err
	}
	return modules, nil
}
