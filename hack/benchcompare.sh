#!/usr/bin/env bash
# benchcompare.sh compares the benchmarks of the root package of the tree
# under test, by default the checkout it belongs to, on the RDMA device that
# RDMA_TEST_DEVICE names: rounds of every benchmark on each engine, the
# samples of which benchstat sets side by side, and, given a baseline
# commit, rounds of the native engine on the baseline before and after the
# tree under test, which make the noise of the baseline and the change from
# it. The summary goes to summary.md in the -out directory, which must not
# exist yet (default: a new temporary one), next to the output of the
# passes, and to standard output. A pass that fails, runs no benchmark or
# outlasts -timeout seconds ends the comparison without a summary.
set -euo pipefail

usage='usage: hack/benchcompare.sh [-base commit] [-rounds n] [-out dir] [-timeout seconds] [tree]'

die() {
	echo "benchcompare: $*" >&2
	exit 1
}

usage_error() {
	echo "$usage" >&2
	exit 2
}

base='' rounds=10 out='' timeout=300
while (($#)); do
	case $1 in
	-base) base=${2-} ;;
	-rounds) rounds=${2-} ;;
	-out) out=${2-} ;;
	-timeout) timeout=${2-} ;;
	-h | -help)
		echo "$usage"
		exit
		;;
	-*) usage_error ;;
	*) break ;;
	esac
	(($# >= 2)) || usage_error
	shift 2
done
(($# <= 1)) || usage_error
[[ $rounds =~ ^[1-9][0-9]*$ && $timeout =~ ^[1-9][0-9]*$ ]] || usage_error
[[ ${RDMA_TEST_DEVICE:-} ]] || die "RDMA_TEST_DEVICE names no RDMA device: set it to the device to measure on, e.g. RDMA_TEST_DEVICE=rxe0"

# The benchmarks fail rather than skip, so that no table has holes.
export RDMA_TEST_REQUIRE=1
# sort and comm agree on the order of names.
export LC_ALL=C

root=$(cd "$(dirname "$0")/.." && pwd -P)
tree=$(cd "${1:-$root}" && pwd -P)
tree_commit=$(git -C "$tree" log -1 --format='%h %s' 2>/dev/null) || tree_commit=$tree
dirty=$(git -C "$tree" status --porcelain 2>/dev/null) || :
[[ -z $dirty ]] || tree_commit+=", with uncommitted changes"
base_commit=none
if [[ $base ]]; then
	base=$(git -C "$tree" rev-parse --verify --quiet "$base^{commit}") || die "$base names no commit of $tree"
	base_commit=$(git -C "$tree" log -1 --format='%h %s' "$base")
	# A baseline with the tree under test's files leaves the engines alone to compare.
	if [[ -z $dirty && $(git -C "$tree" rev-parse "$base^{tree}") == $(git -C "$tree" rev-parse 'HEAD^{tree}') ]]; then
		base_commit+=", the tree under test itself"
		base=''
	fi
fi
if [[ $out ]]; then
	mkdir "$out"
else
	out=$(mktemp -d "${TMPDIR:-/tmp}/benchcompare.XXXXXX")
fi
out=$(cd "$out" && pwd -P)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "benchcompare: building the tree under test, $tree" >&2
(cd "$tree" && go test -c -o "$work/test.test" .) || die "the tree under test does not build"
if [[ $base ]]; then
	echo "benchcompare: building the baseline, $base_commit" >&2
	mkdir "$work/base"
	git -C "$tree" archive "$base" | tar -x -C "$work/base"
	(cd "$work/base" && go test -c -o "$work/base.test" .) || die "the baseline does not build"
fi

# alive waits up to seconds for the process pid to end and returns whether
# it has not. It looks a tenth of a second after it starts and then every
# second.
alive() {
	local pid=$1 deadline=$((SECONDS + $2)) nap=0.1
	while kill -0 "$pid" 2>/dev/null; do
		((SECONDS < deadline)) || return 0
		sleep "$nap"
		nap=1
	done
	return 1
}

# pass runs the benchmarks that match filter with the test binary bin in dir,
# the root of its tree, and further flags, and appends what they print to
# the file group. A pass that fails, runs no benchmark or outlasts the
# timeout ends the comparison with its output. One that hangs gets SIGQUIT,
# on which Go prints its goroutines and exits, and SIGKILL ten seconds
# later; ten seconds after that the comparison ends anyway.
pass() {
	local what=$1 group=$2 bin=$3 dir=$4 filter=$5 pid status=0
	shift 5
	echo "benchcompare: $what" >&2
	(cd "$dir" && exec "$bin" -test.run '^$' -test.bench "$filter" -test.benchtime 500ms -test.count 1 "$@") >"$work/pass.txt" 2>&1 &
	pid=$!
	if alive "$pid" "$timeout"; then
		kill -QUIT "$pid" 2>/dev/null || :
		if alive "$pid" 10; then
			kill -KILL "$pid" 2>/dev/null || :
			alive "$pid" 10 || :
		fi
		cat "$work/pass.txt" >&2
		die "$what did not finish within ${timeout}s"
	fi
	wait "$pid" || status=$?
	if ((status != 0)); then
		cat "$work/pass.txt" >&2
		die "$what failed with exit status $status"
	fi
	if ! grep -q '^Benchmark' "$work/pass.txt"; then
		cat "$work/pass.txt" >&2
		die "$what ran no benchmark"
	fi
	cat "$work/pass.txt" >>"$group"
}

# table writes a table of benchstat's under a heading.
table() {
	local heading=$1 caption=$2
	shift 2
	printf '\n### %s\n\n%s\n\n```\n' "$heading" "$caption"
	(cd "$root" && go tool benchstat "$@")
	printf '```\n'
}

# names lists the benchmarks that ran in the files.
names() {
	awk '/^Benchmark/ {print $1}' "$@" | sort -u
}

cpu="$(lscpu | awk -F ': *' '$1 == "Model name" && !n++ {print $2}'), $(nproc) CPUs"
librdmacm=$(PATH=$PATH:/sbin:/usr/sbin ldconfig -p | awk '$1 == "librdmacm.so.1" && !n++ {print $NF}')
if [[ $librdmacm ]]; then
	librdmacm=$(basename "$(readlink -f "$librdmacm")")
	rdma_core="${librdmacm#librdmacm.so.1.*.} ($librdmacm)"
else
	rdma_core="no librdmacm.so.1"
fi
load_before=$(cut -d ' ' -f 1-3 /proc/loadavg)

native='^BenchmarkRXE//^engine=native$'
pass "the profile of the native engine on the tree under test" "$out/profile.txt" "$work/test.test" "$tree" "$native" \
	-test.cpuprofile "$out/cpu.pprof"
for ((round = 1; round <= rounds; round++)); do
	if [[ $base ]]; then
		pass "round $round of $rounds: the native engine on the baseline" "$out/base-1.txt" "$work/base.test" "$work/base" "$native"
		pass "round $round of $rounds: the native engine on the tree under test" "$out/test.txt" "$work/test.test" "$tree" "$native"
		pass "round $round of $rounds: the native engine on the baseline again" "$out/base-2.txt" "$work/base.test" "$work/base" "$native"
	fi
	pass "round $round of $rounds: every engine on the tree under test" "$out/engines.txt" "$work/test.test" "$tree" '^BenchmarkRXE'
done

load_after=$(cut -d ' ' -f 1-3 /proc/loadavg)
{
	echo "## Benchmarks on $RDMA_TEST_DEVICE"
	echo
	echo "| | |"
	echo "| --- | --- |"
	echo "| Tree under test | $tree_commit |"
	echo "| Baseline | $base_commit |"
	echo "| Rounds | $rounds |"
	echo "| CPU | $cpu |"
	echo "| Kernel | $(uname -r) |"
	echo "| Go | $(go version) |"
	echo "| rdma-core | $rdma_core |"
	echo "| Load average before | $load_before |"
	echo "| Load average after | $load_after |"
	table Implementations "The engines on the tree under test, against rstream in time and throughput and against librdmacm in the metrics rstream lacks." \
		-col '/engine@(rstream librdmacm native)' "$out/engines.txt"
	if [[ $base ]]; then
		# Only the benchmarks that both trees ran make these tables.
		comm -12 <(names "$out/base-1.txt" "$out/base-2.txt") <(names "$out/test.txt") >"$work/both"
		for group in base-1 base-2 test; do
			awk 'NR == FNR {both[$1]; next} !/^Benchmark/ || $1 in both' "$work/both" "$out/$group.txt" >"$work/$group.txt"
		done
		cat "$work/base-1.txt" "$work/base-2.txt" >"$work/base.txt"
		table Noise "The native engine on the baseline after the tree under test, base-2, against it before, base-1." \
			base-1="$work/base-1.txt" base-2="$work/base-2.txt"
		table Change "The native engine on the tree under test against it on the baseline, base-1 and base-2 together." \
			base="$work/base.txt" test="$work/test.txt"
	fi
} >"$out/summary.md"
cat "$out/summary.md"
echo "benchcompare: the summary and the output of the passes are in $out" >&2
