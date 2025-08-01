// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Tetragon

//go:build !windows

package tracing

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"path"

	"github.com/cilium/ebpf"
	"github.com/cilium/tetragon/pkg/api/ops"
	"github.com/cilium/tetragon/pkg/api/tracingapi"
	"github.com/cilium/tetragon/pkg/bpf"
	"github.com/cilium/tetragon/pkg/cgtracker"
	"github.com/cilium/tetragon/pkg/config"
	"github.com/cilium/tetragon/pkg/eventhandler"
	"github.com/cilium/tetragon/pkg/grpc/tracing"
	"github.com/cilium/tetragon/pkg/idtable"
	"github.com/cilium/tetragon/pkg/k8s/apis/cilium.io/v1alpha1"
	"github.com/cilium/tetragon/pkg/kernels"
	"github.com/cilium/tetragon/pkg/logger"
	"github.com/cilium/tetragon/pkg/logger/logfields"
	"github.com/cilium/tetragon/pkg/metrics/enforcermetrics"
	"github.com/cilium/tetragon/pkg/observer"
	"github.com/cilium/tetragon/pkg/option"
	"github.com/cilium/tetragon/pkg/policyfilter"
	"github.com/cilium/tetragon/pkg/reader/network"
	"github.com/cilium/tetragon/pkg/selectors"
	"github.com/cilium/tetragon/pkg/sensors"
	"github.com/cilium/tetragon/pkg/sensors/base"
	"github.com/cilium/tetragon/pkg/sensors/program"
	"github.com/cilium/tetragon/pkg/syscallinfo"
	"github.com/cilium/tetragon/pkg/tracepoint"

	gt "github.com/cilium/tetragon/pkg/generictypes"
)

const (
	// nolint We probably want to keep this even though it's unused at the moment
	// NB: this should match the size of ->args[] of the output message
	genericTP_OutputSize = 9000
)

var (
	genericTracepointTable idtable.Table

	tracepointLog logger.FieldLogger
)

type observerTracepointSensor struct {
	name string
}

func init() {
	tp := &observerTracepointSensor{
		name: "tracepoint sensor",
	}
	sensors.RegisterProbeType("generic_tracepoint", tp)
	observer.RegisterEventHandlerAtInit(ops.MSG_OP_GENERIC_TRACEPOINT, handleGenericTracepoint)
}

// genericTracepoint is the internal representation of a tracepoint
type genericTracepoint struct {
	tableId idtable.EntryID

	Info *tracepoint.Tracepoint
	args []genericTracepointArg

	Spec     *v1alpha1.TracepointSpec
	policyID policyfilter.PolicyID

	// for tracepoints that have a GetUrl or DnsLookup action, we store the table of arguments.
	actionArgs idtable.Table

	pinPathPrefix string

	// policyName is the name of the policy that this tracepoint belongs to
	policyName string

	// message field of the Tracing Policy
	message string

	// tags field of the Tracing Policy
	tags []string

	// parsed kernel selector state
	selectors *selectors.KernelSelectorState

	// custom event handler
	customHandler eventhandler.Handler

	// is raw tracepoint
	raw bool
}

func (tp *genericTracepoint) SetID(id idtable.EntryID) {
	tp.tableId = id
}

// genericTracepointArg is the internal representation of an output value of a
// generic tracepoint.
type genericTracepointArg struct {
	CtxOffset int    // offset within tracepoint ctx
	ArgIdx    uint32 // index in genericTracepoint.args
	TpIdx     int    // index in the tracepoint arguments

	// Meta field: the user defines the meta argument in terms of the
	// tracepoint arguments (MetaTp), but we have to translate it to
	// the ebpf-side arguments (MetaArgIndex).
	// MetaTp
	//  0  -> no metadata information
	//  >0 -> metadata are in the MetaTp of the tracepoint args (1-based)
	//  -1 -> metadata are in retprobe
	MetaTp  int
	MetaArg int

	// this is true if the argument is need to be read, but it's not going
	// to be part of the output. This is needed for arguments that hold
	// metadata but are not part of the output.
	nopTy bool

	// format of the field
	format *tracepoint.FieldFormat

	// bpf generic type
	genericTypeId int

	// user type overload
	userType string

	// data for config.BTFArg
	btf [tracingapi.MaxBTFArgDepth]tracingapi.ConfigBTFArg
}

func genericTracepointTableGet(id idtable.EntryID) (*genericTracepoint, error) {
	entry, err := genericTracepointTable.GetEntry(id)
	if err != nil {
		return nil, fmt.Errorf("getting entry from genericTracepointTable failed with: %w", err)
	}
	val, ok := entry.(*genericTracepoint)
	if !ok {
		return nil, fmt.Errorf("getting entry from genericTracepointTable failed with: got invalid type: %T (%v)", entry, entry)
	}
	return val, nil
}

func (out *genericTracepointArg) String() string {
	return fmt.Sprintf("genericTracepointArg{CtxOffset: %d format: %+v}", out.CtxOffset, out.format)
}

// getGenericTypeId: returns the generic type Id of a tracepoint argument
// if such an id cannot be termined, it returns an GenericInvalidType and an error
func (out *genericTracepointArg) getGenericTypeId() (int, error) {

	if out.userType != "" && out.userType != "auto" {
		if out.userType == "const_buf" {
			// const_buf type depends on the .format.field.Type to decode the result, so
			// disallow it.
			return gt.GenericInvalidType, errors.New("const_buf type cannot be user-defined")
		}
		return gt.GenericTypeFromString(out.userType), nil
	}

	if out.format == nil {
		return gt.GenericInvalidType, errors.New("format is nil")
	}

	if out.format.Field == nil {
		err := out.format.ParseField()
		if err != nil {
			return gt.GenericInvalidType, fmt.Errorf("failed to parse field: %w", err)
		}
	}

	switch ty := out.format.Field.Type.(type) {
	case tracepoint.IntTy:
		if out.format.Size == 4 && out.format.IsSigned {
			return gt.GenericS32Type, nil
		} else if out.format.Size == 4 && !out.format.IsSigned {
			return gt.GenericU32Type, nil
		} else if out.format.Size == 8 && out.format.IsSigned {
			return gt.GenericS64Type, nil
		} else if out.format.Size == 8 && !out.format.IsSigned {
			return gt.GenericU64Type, nil
		}
	case tracepoint.PointerTy:
		// char *
		intTy, ok := ty.Ty.(tracepoint.IntTy)
		if !ok {
			return gt.GenericInvalidType, fmt.Errorf("cannot handle pointer type to %T", ty)
		}
		if intTy.Base == tracepoint.IntTyChar {
			// NB: there is no way to determine if this is a string
			// or a buffer without user information or something we
			// build manually ourselves. For now, we only deal with
			// buffers and expect a metadata argument.
			if out.MetaTp == 0 {
				return gt.GenericInvalidType, errors.New("no metadata field for buffer")
			}
			return gt.GenericCharBuffer, nil
		}

	// NB: we handle array types as constant buffers for now. We copy the
	// data to user-space, and decode them there.
	case tracepoint.ArrayTy:
		nbytes, err := ty.NBytes()
		if err != nil {
			return gt.GenericInvalidType, fmt.Errorf("failed to get size of array type %w", err)
		}
		if out.MetaArg == 0 {
			// set MetaArg equal to the number of bytes we need to copy
			out.MetaArg = nbytes
		}
		return gt.GenericConstBuffer, nil

	case tracepoint.SizeTy:
		return gt.GenericSizeType, nil
	}

	return gt.GenericInvalidType, fmt.Errorf("unknown type: %T", out.format.Field.Type)
}

func buildGenericTracepointArgs(tp *tracepoint.Tracepoint, specArgs []v1alpha1.KProbeArg, raw bool) ([]genericTracepointArg, error) {
	if raw {
		return buildArgsRaw(tp, specArgs)
	}

	if err := tp.LoadFormat(); err != nil {
		return nil, fmt.Errorf("tracepoint %s/%s not supported: %w", tp.Subsys, tp.Event, err)
	}
	return buildArgs(tp, specArgs)
}

func buildArgs(info *tracepoint.Tracepoint, specArgs []v1alpha1.KProbeArg) ([]genericTracepointArg, error) {
	ret := make([]genericTracepointArg, 0, len(specArgs))
	nfields := uint32(len(info.Format.Fields))

	var err error

	for argIdx := range specArgs {
		specArg := &specArgs[argIdx]
		if specArg.Index >= nfields {
			return nil, fmt.Errorf("tracepoint %s/%s has %d fields but field %d was requested", info.Subsys, info.Event, nfields, specArg.Index)
		}
		field := info.Format.Fields[specArg.Index]

		tpArg := genericTracepointArg{
			CtxOffset: int(field.Offset),
			ArgIdx:    uint32(argIdx),
			TpIdx:     int(specArg.Index),
			MetaTp:    getTracepointMetaValue(specArg),
			nopTy:     false,
			format:    &field,
			userType:  specArg.Type,
		}

		tpArg.genericTypeId, err = tpArg.getGenericTypeId()
		if err != nil {
			return nil, fmt.Errorf("output argument %v unsupported: %w", tpArg, err)
		}
		ret = append(ret, tpArg)
	}

	// getOrAppendMeta is a helper function for meta arguments now that we
	// have the configured arguments, we also need to configure meta
	// arguments. Some of them will exist already, but others we will have
	// to create with a nop type so that they will be fetched, but not be
	// part of the output
	getOrAppendMeta := func(metaTp int) (*genericTracepointArg, error) {
		tpIdx := metaTp - 1
		for i := range ret {
			if ret[i].TpIdx == tpIdx {
				return &ret[i], nil
			}
		}

		if tpIdx >= int(nfields) {
			return nil, fmt.Errorf("tracepoint %s/%s has %d fields but field %d was requested in a metadata argument", info.Subsys, info.Event, len(info.Format.Fields), tpIdx)
		}
		field := info.Format.Fields[tpIdx]
		argIdx := uint32(len(ret))
		tpArg := genericTracepointArg{
			CtxOffset:     int(field.Offset),
			ArgIdx:        argIdx,
			TpIdx:         tpIdx,
			MetaTp:        0,
			MetaArg:       0,
			nopTy:         true,
			format:        &field,
			genericTypeId: gt.GenericInvalidType,
		}
		tpArg.genericTypeId, err = tpArg.getGenericTypeId()
		if err != nil {
			return nil, fmt.Errorf("output argument %v unsupported: %w", tpArg, err)
		}
		ret = append(ret, tpArg)
		return &ret[argIdx], nil
	}

	for idx := range ret {
		meta := ret[idx].MetaTp
		if meta == 0 || meta == -1 {
			continue
		}
		a, err := getOrAppendMeta(meta)
		if err != nil {
			return nil, err
		}
		ret[idx].MetaArg = int(a.ArgIdx) + 1
	}
	return ret, nil
}

func buildArgsRaw(info *tracepoint.Tracepoint, specArgs []v1alpha1.KProbeArg) ([]genericTracepointArg, error) {
	ret := make([]genericTracepointArg, 0, len(specArgs))
	for i, tpArg := range specArgs {
		var btf [tracingapi.MaxBTFArgDepth]tracingapi.ConfigBTFArg

		if tpArg.Index > 5 {
			return nil, fmt.Errorf("raw tracepoint (%s/%s) can read up to %d arguments, but %d was requested",
				info.Subsys, info.Event, 5, tpArg.Index)
		}

		arg := genericTracepointArg{
			ArgIdx:   uint32(i),
			TpIdx:    int(tpArg.Index),
			MetaTp:   getTracepointMetaValue(&tpArg),
			userType: tpArg.Type,
		}

		argType, err := arg.getGenericTypeId()
		if err != nil {
			return nil, fmt.Errorf("output argument %v unsupported: %w", tpArg, err)
		}

		if tpArg.Resolve != "" {
			if !bpf.HasProgramLargeSize() {
				return nil, errors.New("error: Resolve flag can be used on v5.4 kernel or higher")
			}
			fn := "__bpf_trace_" + info.Event

			lastBTFType, btfArg, err := resolveBTFArg(fn, tpArg, true)
			if err != nil {
				return nil, fmt.Errorf("error on hook %q for index %d : %w", fn, tpArg.Index, err)
			}
			btf = btfArg
			argType = findTypeFromBTFType(tpArg, lastBTFType)
		}

		arg.btf = btf
		arg.genericTypeId = argType
		ret = append(ret, arg)
	}
	return ret, nil
}

// createGenericTracepoint creates the genericTracepoint information based on
// the user-provided configuration
func createGenericTracepoint(
	sensorName string,
	conf *v1alpha1.TracepointSpec,
	polInfo *policyInfo,
) (*genericTracepoint, error) {
	if conf == nil {
		return nil, errors.New("failed creating generic tracepoint, conf is nil")
	}

	tp := tracepoint.Tracepoint{
		Subsys: conf.Subsystem,
		Event:  conf.Event,
	}

	msgField, err := getPolicyMessage(conf.Message)
	if errors.Is(err, ErrMsgSyntaxShort) || errors.Is(err, ErrMsgSyntaxEscape) {
		return nil, err
	} else if errors.Is(err, ErrMsgSyntaxLong) {
		logger.GetLogger().Warn(fmt.Sprintf("TracingPolicy 'message' field too long, truncated to %d characters", TpMaxMessageLen), "policy-name", polInfo.name)
	}

	tagsField, err := getPolicyTags(conf.Tags)
	if err != nil {
		return nil, err
	}

	tpArgs, err := buildGenericTracepointArgs(&tp, conf.Args, conf.Raw)
	if err != nil {
		return nil, err
	}

	ret := &genericTracepoint{
		tableId:       idtable.UninitializedEntryID,
		Info:          &tp,
		Spec:          conf,
		args:          tpArgs,
		policyID:      polInfo.policyID,
		policyName:    polInfo.name,
		customHandler: polInfo.customHandler,
		message:       msgField,
		tags:          tagsField,
		raw:           conf.Raw,
	}

	genericTracepointTable.AddEntry(ret)
	ret.pinPathPrefix = sensors.PathJoin(sensorName, fmt.Sprintf("gtp-%d", ret.tableId.ID))
	return ret, nil
}

func tpValidateAndAdjustEnforcerAction(
	sensor *sensors.Sensor,
	tp *v1alpha1.TracepointSpec,
	tpID int,
	policyName string,
	spec *v1alpha1.TracingPolicySpec) error {

	registeredEnforcerMetrics := false
	for _, sel := range tp.Selectors {
		for _, act := range sel.MatchActions {
			if act.Action == "NotifyEnforcer" {
				if len(spec.Enforcers) == 0 {
					return errors.New("NotifyEnforcer action specified, but spec contains no enforcers")
				}

				// EnforcerNotifyActionArgIndex already set, do nothing
				if act.EnforcerNotifyActionArgIndex != nil {
					continue
				}

				switch {
				case tp.Subsystem == "raw_syscalls" && tp.Event == "sys_enter":
					for i, arg := range tp.Args {
						// syscall id
						if arg.Index == 4 {
							val := uint32(i)
							act.EnforcerNotifyActionArgIndex = &val
						}
					}
					defaultABI, _ := syscallinfo.DefaultABI()
					enforcermetrics.RegisterInfo(policyName, uint32(tpID), func(arg uint32) string {
						syscallID := parseSyscall64Value(uint64(arg))
						sysName, _ := syscallinfo.GetSyscallName(syscallID.ABI, int(syscallID.ID))
						if sysName == "" {
							sysName = fmt.Sprintf("syscall-%d", syscallID.ID)
						}
						if syscallID.ABI != defaultABI {
							sysName = fmt.Sprintf("%s/%s", syscallID.ABI, sysName)
						}
						return sysName
					})
					registeredEnforcerMetrics = true
				default:
					enforcermetrics.RegisterInfo(policyName, uint32(tpID), func(_ uint32) string {
						return fmt.Sprintf("%s/%s", tp.Subsystem, tp.Event)
					})

				}
			}
		}
	}

	if registeredEnforcerMetrics {
		sensor.AddPostUnloadHook(func() error {
			enforcermetrics.UnregisterPolicy(policyName)
			return nil
		})
	}

	return nil
}

// createGenericTracepointSensor will create a sensor that can be loaded based on a generic tracepoint configuration
func createGenericTracepointSensor(
	spec *v1alpha1.TracingPolicySpec,
	name string,
	polInfo *policyInfo,
) (*sensors.Sensor, error) {
	confs := spec.Tracepoints
	lists := spec.Lists

	ret := &sensors.Sensor{
		Name:      name,
		Policy:    polInfo.name,
		Namespace: polInfo.namespace,
	}

	tracepoints := make([]*genericTracepoint, 0, len(confs))
	for i, tpSpec := range confs {
		err := tpValidateAndAdjustEnforcerAction(ret, &tpSpec, i, polInfo.name, spec)
		if err != nil {
			return nil, err
		}
		tp, err := createGenericTracepoint(name, &tpSpec, polInfo)
		if err != nil {
			return nil, err
		}
		tracepoints = append(tracepoints, tp)
	}

	has := hasMaps{
		enforcer: len(spec.Enforcers) != 0,
	}

	maps := []*program.Map{}
	progs := make([]*program.Program, 0, len(tracepoints))
	for _, tp := range tracepoints {
		pinProg := sensors.PathJoin(fmt.Sprintf("%s:%s", tp.Info.Subsys, tp.Info.Event))
		attach := fmt.Sprintf("%s/%s", tp.Info.Subsys, tp.Info.Event)
		label := "tracepoint/generic_tracepoint"
		if tp.raw {
			label = "raw_tp/generic_tracepoint"
		}
		prog0 := program.Builder(
			path.Join(option.Config.HubbleLib, config.GenericTracepointObjs(tp.raw)),
			attach,
			label,
			pinProg,
			"generic_tracepoint",
		).SetPolicy(polInfo.name)

		err := tp.InitKernelSelectors(lists)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize tracepoint kernel selectors: %w", err)
		}

		has.fdInstall = selectorsHaveFDInstall(tp.Spec.Selectors)

		prog0.LoaderData = tp.tableId
		progs = append(progs, prog0)

		fdinstall := program.MapBuilderSensor("fdinstall_map", prog0)
		if has.fdInstall {
			fdinstall.SetMaxEntries(fdInstallMapMaxEntries)
		}
		maps = append(maps, fdinstall)

		tailCalls := program.MapBuilderProgram("tp_calls", prog0)
		maps = append(maps, tailCalls)

		filterMap := program.MapBuilderProgram("filter_map", prog0)
		maps = append(maps, filterMap)

		argFilterMaps := program.MapBuilderProgram("argfilter_maps", prog0)
		if !kernels.MinKernelVersion("5.9") {
			// Versions before 5.9 do not allow inner maps to have different sizes.
			// See: https://lore.kernel.org/bpf/20200828011800.1970018-1-kafai@fb.com/
			maxEntries := tp.selectors.ValueMapsMaxEntries()
			argFilterMaps.SetInnerMaxEntries(maxEntries)
		}
		maps = append(maps, argFilterMaps)

		addr4FilterMaps := program.MapBuilderProgram("addr4lpm_maps", prog0)
		if !kernels.MinKernelVersion("5.9") {
			// Versions before 5.9 do not allow inner maps to have different sizes.
			// See: https://lore.kernel.org/bpf/20200828011800.1970018-1-kafai@fb.com/
			maxEntries := tp.selectors.Addr4MapsMaxEntries()
			addr4FilterMaps.SetInnerMaxEntries(maxEntries)
		}
		maps = append(maps, addr4FilterMaps)

		addr6FilterMaps := program.MapBuilderProgram("addr6lpm_maps", prog0)
		if !kernels.MinKernelVersion("5.9") {
			// Versions before 5.9 do not allow inner maps to have different sizes.
			// See: https://lore.kernel.org/bpf/20200828011800.1970018-1-kafai@fb.com/
			maxEntries := tp.selectors.Addr6MapsMaxEntries()
			addr6FilterMaps.SetInnerMaxEntries(maxEntries)
		}
		maps = append(maps, addr6FilterMaps)

		numSubMaps := selectors.StringMapsNumSubMaps
		if !kernels.MinKernelVersion("5.11") {
			numSubMaps = selectors.StringMapsNumSubMapsSmall
		}
		for string_map_index := range numSubMaps {
			stringFilterMap := program.MapBuilderProgram(fmt.Sprintf("string_maps_%d", string_map_index), prog0)
			if !kernels.MinKernelVersion("5.9") {
				// Versions before 5.9 do not allow inner maps to have different sizes.
				// See: https://lore.kernel.org/bpf/20200828011800.1970018-1-kafai@fb.com/
				maxEntries := tp.selectors.StringMapsMaxEntries(string_map_index)
				stringFilterMap.SetInnerMaxEntries(maxEntries)
			}
			maps = append(maps, stringFilterMap)
		}

		stringPrefixFilterMaps := program.MapBuilderProgram("string_prefix_maps", prog0)
		if !kernels.MinKernelVersion("5.9") {
			// Versions before 5.9 do not allow inner maps to have different sizes.
			// See: https://lore.kernel.org/bpf/20200828011800.1970018-1-kafai@fb.com/
			maxEntries := tp.selectors.StringPrefixMapsMaxEntries()
			stringPrefixFilterMaps.SetInnerMaxEntries(maxEntries)
		}
		maps = append(maps, stringPrefixFilterMaps)

		stringPostfixFilterMaps := program.MapBuilderProgram("string_postfix_maps", prog0)
		if !kernels.MinKernelVersion("5.9") {
			// Versions before 5.9 do not allow inner maps to have different sizes.
			// See: https://lore.kernel.org/bpf/20200828011800.1970018-1-kafai@fb.com/
			maxEntries := tp.selectors.StringPostfixMapsMaxEntries()
			stringPostfixFilterMaps.SetInnerMaxEntries(maxEntries)
		}
		maps = append(maps, stringPostfixFilterMaps)

		matchBinariesPaths := program.MapBuilderProgram("tg_mb_paths", prog0)
		if !kernels.MinKernelVersion("5.9") {
			// Versions before 5.9 do not allow inner maps to have different sizes.
			// See: https://lore.kernel.org/bpf/20200828011800.1970018-1-kafai@fb.com/
			matchBinariesPaths.SetInnerMaxEntries(tp.selectors.MatchBinariesPathsMaxEntries())
		}
		maps = append(maps, matchBinariesPaths)

		if has.enforcer {
			maps = append(maps, enforcerMapsUser(prog0)...)
		}

		if option.Config.EnableCgTrackerID {
			maps = append(maps, program.MapUser(cgtracker.MapName, prog0))
		}

		selMatchBinariesMap := program.MapBuilderProgram("tg_mb_sel_opts", prog0)
		maps = append(maps, selMatchBinariesMap)

		maps = append(maps, polInfo.policyConfMap(prog0))
	}

	maps = append(maps, program.MapUserFrom(base.ExecveMap))

	ret.Progs = progs
	ret.Maps = maps

	ret.DestroyHook = func() error {
		var errs error

		for _, tp := range tracepoints {
			if err := selectors.CleanupKernelSelectorState(tp.selectors); err != nil {
				errs = errors.Join(errs, err)
			}

			_, err := genericTracepointTable.RemoveEntry(tp.tableId)
			if err != nil {
				errs = errors.Join(errs, err)
			}
		}
		return errs
	}

	return ret, nil
}

func (tp *genericTracepoint) InitKernelSelectors(lists []v1alpha1.ListSpec) error {
	if tp.selectors != nil {
		return errors.New("InitKernelSelectors: selectors already initialized")
	}

	// rewrite arg index
	selArgs := make([]v1alpha1.KProbeArg, 0, len(tp.args))
	selSelectors := make([]v1alpha1.KProbeSelector, 0, len(tp.Spec.Selectors))
	for _, origSel := range tp.Spec.Selectors {
		selSelectors = append(selSelectors, *origSel.DeepCopy())
	}

	for _, tpArg := range tp.args {
		selType, err := gt.GenericTypeToString(tpArg.genericTypeId)
		if err != nil {
			return fmt.Errorf("output argument %v type not found: %w", tpArg, err)
		}

		// NB: this a selector argument, meant to be passed to InitKernelSelectors.
		// The only fields needed for the latter are Index and Type
		selArg := v1alpha1.KProbeArg{
			Index: tpArg.ArgIdx,
			Type:  selType,
		}
		selArgs = append(selArgs, selArg)

		// update selectors
		for j, s := range selSelectors {
			for k, match := range s.MatchArgs {
				if match.Index == uint32(tpArg.TpIdx) {
					selSelectors[j].MatchArgs[k].Index = uint32(tpArg.ArgIdx)
				}
			}
		}
	}

	selectors, err := selectors.InitKernelSelectorState(selSelectors, selArgs, &tp.actionArgs, &listReader{lists}, nil)
	if err != nil {
		return err
	}
	tp.selectors = selectors
	return nil
}

func (tp *genericTracepoint) EventConfig() (*tracingapi.EventConfig, error) {

	if len(tp.args) > tracingapi.EventConfigMaxArgs {
		return nil, fmt.Errorf("number of arguments (%d) larger than max (%d)", len(tp.args), tracingapi.EventConfigMaxArgs)
	}

	config := initEventConfig()
	config.PolicyID = uint32(tp.policyID)
	config.FuncId = uint32(tp.tableId.ID)

	if tp.raw {
		return tp.eventConfigRaw(config)
	}
	return tp.eventConfig(config)
}

func (tp *genericTracepoint) eventConfigRaw(config *tracingapi.EventConfig) (*tracingapi.EventConfig, error) {

	// iterate over output arguments
	for i, tpArg := range tp.args {
		config.BTFArg[i] = tpArg.btf
		config.ArgType[i] = int32(tpArg.genericTypeId)
		config.ArgMeta[i] = uint32(tpArg.MetaArg)
		config.ArgIndex[i] = int32(tpArg.TpIdx)

		tracepointLog.Debug(fmt.Sprintf("configured argument #%d: %+v (type:%d)", i, tpArg, tpArg.genericTypeId))
	}
	return config, nil
}

func (tp *genericTracepoint) eventConfig(config *tracingapi.EventConfig) (*tracingapi.EventConfig, error) {

	// iterate over output arguments
	for i, tpArg := range tp.args {
		config.ArgTpCtxOff[i] = uint32(tpArg.CtxOffset)
		config.ArgType[i] = int32(tpArg.genericTypeId)
		config.ArgMeta[i] = uint32(tpArg.MetaArg)
		config.ArgIndex[i] = int32(tpArg.TpIdx)

		tracepointLog.Debug(fmt.Sprintf("configured argument #%d: %+v (type:%d)", i, tpArg, tpArg.genericTypeId))
	}

	return config, nil
}

func LoadGenericTracepointSensor(bpfDir string, load *program.Program, maps []*program.Map, verbose int) error {

	tracepointLog = logger.GetLogger()

	id, ok := load.LoaderData.(idtable.EntryID)
	if !ok {
		return fmt.Errorf("loaderData for genericTracepoint %s is %T (%v) (not an int)", load.Name, load.LoaderData, load.LoaderData)
	}

	tp, err := genericTracepointTableGet(id)
	if err != nil {
		return fmt.Errorf("could not find generic tracepoint information for %s: %w", load.Attach, err)
	}

	load.MapLoad = append(load.MapLoad, selectorsMaploads(tp.selectors, 0)...)

	config, err := tp.EventConfig()
	if err != nil {
		return fmt.Errorf("failed to generate config data for generic tracepoint: %w", err)
	}
	var binBuf bytes.Buffer
	binary.Write(&binBuf, binary.LittleEndian, *config)
	cfg := &program.MapLoad{
		Name: "config_map",
		Load: func(m *ebpf.Map, _ string) error {
			return m.Update(uint32(0), binBuf.Bytes()[:], ebpf.UpdateAny)
		},
	}
	load.MapLoad = append(load.MapLoad, cfg)

	if tp.raw {
		err = program.LoadRawTracepointProgram(bpfDir, load, maps, verbose)
	} else {
		err = program.LoadTracepointProgram(bpfDir, load, maps, verbose)
	}

	if err == nil {
		logger.GetLogger().Info(fmt.Sprintf("Loaded generic tracepoint program: %s -> %s", load.Name, load.Attach))
	}
	return err
}

func handleGenericTracepoint(r *bytes.Reader) ([]observer.Event, error) {
	m := tracingapi.MsgGenericTracepoint{}
	err := binary.Read(r, binary.LittleEndian, &m)
	if err != nil {
		return nil, fmt.Errorf("failed to read tracepoint: %w", err)
	}

	unix := &tracing.MsgGenericTracepointUnix{
		Msg:    &m,
		Subsys: "UNKNOWN",
		Event:  "UNKNOWN",
	}

	tp, err := genericTracepointTableGet(idtable.EntryID{ID: int(m.FuncId)})
	if err != nil {
		logger.GetLogger().Warn("genericTracepoint info not found", "id", m.FuncId, logfields.Error, err)
		return []observer.Event{unix}, nil
	}

	ret, err := handleMsgGenericTracepoint(&m, unix, tp, r)
	if tp.customHandler != nil {
		ret, err = tp.customHandler(ret, err)
	}
	return ret, err
}

func handleMsgGenericTracepoint(
	m *tracingapi.MsgGenericTracepoint,
	unix *tracing.MsgGenericTracepointUnix,
	tp *genericTracepoint,
	r *bytes.Reader,
) ([]observer.Event, error) {

	switch m.ActionId {
	case selectors.ActionTypeGetUrl, selectors.ActionTypeDnsLookup:
		actionArgEntry, err := tp.actionArgs.GetEntry(idtable.EntryID{ID: int(m.ActionArgId)})
		if err != nil {
			logger.GetLogger().Warn(fmt.Sprintf("Failed to find argument for id:%d", m.ActionArgId), logfields.Error, err)
			return nil, errors.New("failed to find argument for id")
		}
		actionArg := actionArgEntry.(*selectors.ActionArgEntry).GetArg()
		switch m.ActionId {
		case selectors.ActionTypeGetUrl:
			logger.Trace(logger.GetLogger(), "Get URL Action", "URL", actionArg)
			getUrl(actionArg)
		case selectors.ActionTypeDnsLookup:
			logger.Trace(logger.GetLogger(), "DNS lookup", "FQDN", actionArg)
			dnsLookup(actionArg)
		}
	}

	unix.Subsys = tp.Info.Subsys
	unix.Event = tp.Info.Event
	unix.PolicyName = tp.policyName
	unix.Message = tp.message
	unix.Tags = tp.tags

	for idx, out := range tp.args {

		if out.nopTy {
			continue
		}

		switch out.genericTypeId {
		case gt.GenericU64Type:
			var val uint64
			err := binary.Read(r, binary.LittleEndian, &val)
			if err != nil {
				logger.GetLogger().Warn(fmt.Sprintf("Size type error sizeof %d", m.Common.Size), logfields.Error, err)
			}
			unix.Args = append(unix.Args, val)

		case gt.GenericS64Type:
			var val int64
			err := binary.Read(r, binary.LittleEndian, &val)
			if err != nil {
				logger.GetLogger().Warn(fmt.Sprintf("Size type error sizeof %d", m.Common.Size), logfields.Error, err)
			}
			unix.Args = append(unix.Args, val)

		case gt.GenericU32Type:
			var val uint32
			err := binary.Read(r, binary.LittleEndian, &val)
			if err != nil {
				logger.GetLogger().Warn(fmt.Sprintf("Size type error sizeof %d", m.Common.Size), logfields.Error, err)
			}
			unix.Args = append(unix.Args, val)

		case gt.GenericIntType, gt.GenericS32Type:
			var val int32
			err := binary.Read(r, binary.LittleEndian, &val)
			if err != nil {
				logger.GetLogger().Warn(fmt.Sprintf("Size type error sizeof %d", m.Common.Size), logfields.Error, err)
			}
			unix.Args = append(unix.Args, val)

		case gt.GenericSizeType:
			var val uint64

			err := binary.Read(r, binary.LittleEndian, &val)
			if err != nil {
				logger.GetLogger().Warn(fmt.Sprintf("Size type error sizeof %d", m.Common.Size), logfields.Error, err)
			}
			unix.Args = append(unix.Args, val)

		case gt.GenericCharBuffer, gt.GenericCharIovec:
			if arg, err := ReadArgBytes(r, idx, false); err == nil {
				unix.Args = append(unix.Args, arg.Value)
			} else {
				logger.GetLogger().Warn("failed to read bytes argument", logfields.Error, err)
			}

		case gt.GenericConstBuffer:
			if out.format == nil {
				logger.GetLogger().Warn("GenericConstBuffer lacks format. Cannot decode argument")
				break
			}
			if arrTy, ok := out.format.Field.Type.(tracepoint.ArrayTy); ok {
				intTy, ok := arrTy.Ty.(tracepoint.IntTy)
				if !ok {
					logger.GetLogger().Warn("failed to read array argument: expecting array of integers")
					break
				}

				switch intTy.Base {
				case tracepoint.IntTyLong:
					var val uint64
					for i := range arrTy.Size {
						err := binary.Read(r, binary.LittleEndian, &val)
						if err != nil {
							logger.GetLogger().Warn(fmt.Sprintf("failed to read element %d from array", i), logfields.Error, err)
							return nil, err
						}
						unix.Args = append(unix.Args, val)
					}
				default:
					logger.GetLogger().Warn(fmt.Sprintf("failed to read array argument: unexpected base type: %d", intTy.Base))
				}
			}
		case gt.GenericStringType, gt.GenericDataLoc:
			if arg, err := parseString(r); err != nil {
				logger.GetLogger().Warn("error parsing arg type string", logfields.Error, err)
			} else {
				unix.Args = append(unix.Args, arg)
			}
		case gt.GenericSkbType:
			var skb tracingapi.MsgGenericKprobeSkb
			var arg tracingapi.MsgGenericKprobeArgSkb

			err := binary.Read(r, binary.LittleEndian, &skb)
			if err != nil {
				logger.GetLogger().Warn("skb type err", logfields.Error, err)
			}

			arg.Hash = skb.Hash
			arg.Len = skb.Len
			arg.Priority = skb.Priority
			arg.Mark = skb.Mark
			arg.Family = skb.Tuple.Family
			arg.Saddr = network.GetIP(skb.Tuple.Saddr, skb.Tuple.Family).String()
			arg.Daddr = network.GetIP(skb.Tuple.Daddr, skb.Tuple.Family).String()
			arg.Sport = uint32(skb.Tuple.Sport)
			arg.Dport = uint32(skb.Tuple.Dport)
			arg.Proto = uint32(skb.Tuple.Protocol)
			arg.SecPathLen = skb.SecPathLen
			arg.SecPathOLen = skb.SecPathOLen
			unix.Args = append(unix.Args, arg)
		case gt.GenericSockType, gt.GenericSocketType:
			var sock tracingapi.MsgGenericKprobeSock
			var arg tracingapi.MsgGenericKprobeArgSock

			err := binary.Read(r, binary.LittleEndian, &sock)
			if err != nil {
				logger.GetLogger().Warn("sock type err", logfields.Error, err)
			}

			arg.Family = sock.Tuple.Family
			arg.State = sock.State
			arg.Type = sock.Type
			arg.Protocol = sock.Tuple.Protocol
			arg.Mark = sock.Mark
			arg.Priority = sock.Priority
			arg.Saddr = network.GetIP(sock.Tuple.Saddr, sock.Tuple.Family).String()
			arg.Daddr = network.GetIP(sock.Tuple.Daddr, sock.Tuple.Family).String()
			arg.Sport = uint32(sock.Tuple.Sport)
			arg.Dport = uint32(sock.Tuple.Dport)
			arg.Sockaddr = sock.Sockaddr
			unix.Args = append(unix.Args, arg)

		case gt.GenericSockaddrType:
			var address tracingapi.MsgGenericKprobeSockaddr
			var arg tracingapi.MsgGenericKprobeArgSockaddr

			err := binary.Read(r, binary.LittleEndian, &address)
			if err != nil {
				logger.GetLogger().Warn("sockaddr type err", logfields.Error, err)
			}

			arg.SinFamily = address.SinFamily
			arg.SinAddr = network.GetIP(address.SinAddr, address.SinFamily).String()
			arg.SinPort = uint32(address.SinPort)
			unix.Args = append(unix.Args, arg)

		case gt.GenericSyscall64:
			var val uint64
			err := binary.Read(r, binary.LittleEndian, &val)
			if err != nil {
				logger.GetLogger().Warn(fmt.Sprintf("Size type error sizeof %d", m.Common.Size), logfields.Error, err)
			}
			if option.Config.CompatibilitySyscall64SizeType {
				// NB: clear Is32Bit to mantain previous behaviour
				val = val & (^uint64(Is32Bit))
				unix.Args = append(unix.Args, val)
			} else {
				val := parseSyscall64Value(val)
				unix.Args = append(unix.Args, val)
			}

		case gt.GenericLinuxBinprmType:
			var arg tracingapi.MsgGenericKprobeArgLinuxBinprm
			var flags uint32
			var mode uint16
			var err error

			arg.Value, err = parseString(r)
			if err != nil {
				if errors.Is(err, errParseStringSize) {
					arg.Value = "/"
				} else {
					logger.GetLogger().Warn("error parsing arg type linux_binprm")
				}
			}

			err = binary.Read(r, binary.LittleEndian, &flags)
			if err != nil {
				flags = 0
			}

			err = binary.Read(r, binary.LittleEndian, &mode)
			if err != nil {
				mode = 0
			}
			arg.Flags = flags
			arg.Permission = mode
			unix.Args = append(unix.Args, arg)

		case gt.GenericFileType, gt.GenericFdType, gt.GenericKiocb:
			var arg tracingapi.MsgGenericKprobeArgFile
			var flags uint32
			var b int32
			var mode uint16
			var err error

			/* Eat file descriptor its not used in userland */
			if out.genericTypeId == gt.GenericFdType {
				binary.Read(r, binary.LittleEndian, &b)
			}

			arg.Value, err = parseString(r)
			if err != nil {
				if errors.Is(err, errParseStringSize) {
					// If no size then path walk was not possible and file was
					// either a mount point or not a "file" at all which can
					// happen if running without any filters and kernel opens an
					// anonymous inode. For this lets just report its on "/" all
					// though pid filtering will mostly catch this.
					arg.Value = "/"
				} else {
					logger.GetLogger().Warn("error parsing arg type file", logfields.Error, err)
				}
			}

			// read the first byte that keeps the flags
			err = binary.Read(r, binary.LittleEndian, &flags)
			if err != nil {
				flags = 0
			}

			if out.genericTypeId == gt.GenericFileType || out.genericTypeId == gt.GenericKiocb {
				err = binary.Read(r, binary.LittleEndian, &mode)
				if err != nil {
					mode = 0
				}
				arg.Permission = mode
			}

			arg.Flags = flags
			unix.Args = append(unix.Args, arg)

		default:
			logger.GetLogger().Warn(fmt.Sprintf("handleGenericTracepoint: ignoring:  %+v", out))
		}
	}
	return []observer.Event{unix}, nil
}

func (t *observerTracepointSensor) LoadProbe(args sensors.LoadProbeArgs) error {
	return LoadGenericTracepointSensor(args.BPFDir, args.Load, args.Maps, args.Verbose)
}
