package server_jit

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	flag "github.com/spf13/pflag"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"

	"github.com/offchainlabs/nitro/util"
	"github.com/offchainlabs/nitro/util/stopwaiter"
	"github.com/offchainlabs/nitro/validator"
	"github.com/offchainlabs/nitro/validator/server_common"
)

type JitSpawnerConfig struct {
	Workers          int           `koanf:"workers" reload:"hot"`
	Cranelift        bool          `koanf:"cranelift"`
	MaxExecutionTime time.Duration `koanf:"max-execution-time" reload:"hot"`

	// TODO: change WasmMemoryUsageLimit to a string and use resourcemanager.ParseMemLimit
	WasmMemoryUsageLimit int `koanf:"wasm-memory-usage-limit"`
}

type JitSpawnerConfigFecher func() *JitSpawnerConfig

var DefaultJitSpawnerConfig = JitSpawnerConfig{
	Workers:              0,
	Cranelift:            true,
	WasmMemoryUsageLimit: 4294967296, // 2^32 WASM memory limit
	MaxExecutionTime:     time.Minute * 10,
}

func JitSpawnerConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.Int(prefix+".workers", DefaultJitSpawnerConfig.Workers, "number of concurrent validation threads")
	f.Bool(prefix+".cranelift", DefaultJitSpawnerConfig.Cranelift, "use Cranelift instead of LLVM when validating blocks using the jit-accelerated block validator")
	f.Int(prefix+".wasm-memory-usage-limit", DefaultJitSpawnerConfig.WasmMemoryUsageLimit, "if memory used by a jit wasm exceeds this limit, a warning is logged")
	f.Duration(prefix+".max-execution-time", DefaultJitSpawnerConfig.MaxExecutionTime, "if execution time used by a jit wasm exceeds this limit, a rpc error is returned")
}

type JitSpawner struct {
	stopwaiter.StopWaiter
	count         atomic.Int32
	locator       *server_common.MachineLocator
	machineLoader *JitMachineLoader
	config        JitSpawnerConfigFecher
}

func NewJitSpawner(locator *server_common.MachineLocator, config JitSpawnerConfigFecher, fatalErrChan chan error) (*JitSpawner, error) {
	// TODO - preload machines
	machineConfig := DefaultJitMachineConfig
	machineConfig.JitCranelift = config().Cranelift
	machineConfig.WasmMemoryUsageLimit = config().WasmMemoryUsageLimit
	maxExecutionTime := config().MaxExecutionTime
	loader, err := NewJitMachineLoader(&machineConfig, locator, maxExecutionTime, fatalErrChan)
	if err != nil {
		return nil, err
	}
	spawner := &JitSpawner{
		locator:       locator,
		machineLoader: loader,
		config:        config,
	}
	return spawner, nil
}

func (v *JitSpawner) Start(ctx_in context.Context) error {
	v.StopWaiter.Start(ctx_in, v)
	return nil
}

func (v *JitSpawner) WasmModuleRoots() ([]common.Hash, error) {
	return v.locator.ModuleRoots(), nil
}

func (v *JitSpawner) StylusArchs() []rawdb.WasmTarget {
	return []rawdb.WasmTarget{rawdb.LocalTarget()}
}

func (v *JitSpawner) execute(
	ctx context.Context, entry *validator.ValidationInput, moduleRoot common.Hash,
) (validator.GoGlobalState, error) {
	machine, err := v.machineLoader.GetMachine(ctx, moduleRoot)
	if err != nil {
		return validator.GoGlobalState{}, fmt.Errorf("unable to get WASM machine: %w", err)
	}

	state, err := machine.prove(ctx, entry)
	return state, err
}

func (s *JitSpawner) Name() string {
	if s.config().Cranelift {
		return "jit-cranelift"
	}
	return "jit"
}

func (v *JitSpawner) Launch(entry *validator.ValidationInput, moduleRoot common.Hash) validator.ValidationRun {
	v.count.Add(1)
	promise := stopwaiter.LaunchPromiseThread[validator.GoGlobalState](v, func(ctx context.Context) (validator.GoGlobalState, error) {
		defer v.count.Add(-1)
		return v.execute(ctx, entry, moduleRoot)
	})
	return server_common.NewValRun(promise, moduleRoot)
}

func (v *JitSpawner) Room() int {
	avail := v.config().Workers
	if avail == 0 {
		avail = util.GoMaxProcs()
	}
	return avail
}

func (v *JitSpawner) Stop() {
	v.StopOnly()
	v.machineLoader.Stop()
}
