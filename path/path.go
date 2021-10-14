package path

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KusakabeSi/EtherGuardVPN/config"
	orderedmap "github.com/KusakabeSi/EtherGuardVPN/orderdmap"
	yaml "gopkg.in/yaml.v2"
)

const Infinity = float64(99999)

func (g *IG) GetCurrentTime() time.Time {
	return time.Now().Add(g.ntp_offset).Round(0)
}

type Latency struct {
	ping     float64
	ping_old float64
	time     time.Time
}

type Fullroute struct {
	Next config.NextHopTable `yaml:"NextHopTable"`
	Dist config.DistTable    `yaml:"DistanceTable"`
}

// IG is a graph of integers that satisfies the Graph interface.
type IG struct {
	Vert                      map[config.Vertex]bool
	edges                     map[config.Vertex]map[config.Vertex]*Latency
	edgelock                  *sync.RWMutex
	StaticMode                bool
	JitterTolerance           float64
	JitterToleranceMultiplier float64
	NodeReportTimeout         time.Duration
	SuperNodeInfoTimeout      time.Duration
	RecalculateCoolDown       time.Duration
	TimeoutCheckInterval      time.Duration
	recalculateTime           time.Time
	dlTable                   config.DistTable
	nhTable                   config.NextHopTable
	NhTableHash               [32]byte
	NhTableExpire             time.Time
	IsSuperMode               bool
	loglevel                  config.LoggerInfo

	ntp_wg      sync.WaitGroup
	ntp_info    config.NTPinfo
	ntp_offset  time.Duration
	ntp_servers orderedmap.OrderedMap // serverurl:lentancy
}

func S2TD(secs float64) time.Duration {
	return time.Duration(secs * float64(time.Second))
}

func NewGraph(num_node int, IsSuperMode bool, theconfig config.GraphRecalculateSetting, ntpinfo config.NTPinfo, loglevel config.LoggerInfo) *IG {
	g := IG{
		edgelock:                  &sync.RWMutex{},
		StaticMode:                theconfig.StaticMode,
		JitterTolerance:           theconfig.JitterTolerance,
		JitterToleranceMultiplier: theconfig.JitterToleranceMultiplier,
		NodeReportTimeout:         S2TD(theconfig.NodeReportTimeout),
		RecalculateCoolDown:       S2TD(theconfig.RecalculateCoolDown),
		TimeoutCheckInterval:      S2TD(theconfig.TimeoutCheckInterval),
		ntp_info:                  ntpinfo,
	}
	g.Vert = make(map[config.Vertex]bool, num_node)
	g.edges = make(map[config.Vertex]map[config.Vertex]*Latency, num_node)
	g.IsSuperMode = IsSuperMode
	g.loglevel = loglevel
	g.InitNTP()
	return &g
}

func (g *IG) GetWeightType(x float64) (y float64) {
	x = math.Abs(x)
	y = x
	if g.JitterTolerance > 0.001 && g.JitterToleranceMultiplier > 1 {
		t := g.JitterTolerance
		r := g.JitterToleranceMultiplier
		y = math.Pow(math.Ceil(math.Pow(x/t, 1/r)), r) * t
	}
	return y
}

func (g *IG) ShouldUpdate(u config.Vertex, v config.Vertex, newval float64) bool {
	oldval := math.Abs(g.OldWeight(u, v) * 1000)
	newval = math.Abs(newval * 1000)
	if g.IsSuperMode {
		if g.JitterTolerance > 0.001 && g.JitterToleranceMultiplier >= 1 {
			diff := math.Abs(newval - oldval)
			x := math.Max(oldval, newval)
			t := g.JitterTolerance
			r := g.JitterToleranceMultiplier
			return diff > t+x*(r-1) // https://www.desmos.com/calculator/raoti16r5n
		}
		return oldval == newval
	} else {
		return g.GetWeightType(oldval) != g.GetWeightType(newval)
	}
}

func (g *IG) CheckAnyShouldUpdate() bool {
	vert := g.Vertices()
	for u, _ := range vert {
		for v, _ := range vert {
			newVal := g.Weight(u, v)
			if g.ShouldUpdate(u, v, newVal) {
				return true
			}
		}
	}
	return false
}

func (g *IG) RecalculateNhTable(checkchange bool) (changed bool) {
	if g.StaticMode {
		if bytes.Equal(g.NhTableHash[:], make([]byte, 32)) {
			changed = checkchange
		}
		return
	}
	if !g.ShouldCalculate() {
		return
	}
	if g.recalculateTime.Add(g.RecalculateCoolDown).Before(time.Now()) {
		dist, next, _ := g.FloydWarshall(false)
		changed = false
		if checkchange {
		CheckLoop:
			for src, dsts := range next {
				for dst, old_next := range dsts {
					nexthop := g.Next(src, dst)
					if old_next != nexthop {
						changed = true
						break CheckLoop
					}
				}
			}
		}
		g.dlTable, g.nhTable = dist, next
		g.recalculateTime = time.Now()
	}
	return
}

func (g *IG) RemoveVirt(v config.Vertex, recalculate bool, checkchange bool) (changed bool) { //Waiting for test
	g.edgelock.Lock()
	delete(g.Vert, v)
	delete(g.edges, v)
	for u, _ := range g.edges {
		delete(g.edges[u], v)
	}
	g.edgelock.Unlock()
	g.NhTableHash = [32]byte{}
	if recalculate {
		changed = g.RecalculateNhTable(checkchange)
	}
	return
}

func (g *IG) UpdateLatency(u, v config.Vertex, dt time.Duration, recalculate bool, checkchange bool) (changed bool) {
	g.edgelock.Lock()
	g.Vert[u] = true
	g.Vert[v] = true
	w := float64(dt) / float64(time.Second)
	if _, ok := g.edges[u]; !ok {
		g.recalculateTime = time.Time{}
		g.edges[u] = make(map[config.Vertex]*Latency)
	}
	g.edgelock.Unlock()
	should_update := g.ShouldUpdate(u, v, w)
	g.edgelock.Lock()
	if _, ok := g.edges[u][v]; ok {
		g.edges[u][v].ping = w
		g.edges[u][v].time = time.Now()
	} else {
		g.edges[u][v] = &Latency{
			ping:     w,
			ping_old: Infinity,
			time:     time.Now(),
		}
	}
	g.edgelock.Unlock()
	if should_update && recalculate {
		changed = g.RecalculateNhTable(checkchange)
	}
	return
}
func (g *IG) Vertices() map[config.Vertex]bool {
	vr := make(map[config.Vertex]bool)
	g.edgelock.RLock()
	defer g.edgelock.RUnlock()
	for k, v := range g.Vert { //copy a new list
		vr[k] = v
	}
	return vr
}
func (g IG) Neighbors(v config.Vertex) (vs []config.Vertex) {
	g.edgelock.RLock()
	defer g.edgelock.RUnlock()
	for k := range g.edges[v] { //copy a new list
		vs = append(vs, k)
	}
	return vs
}

func (g *IG) Next(u, v config.Vertex) *config.Vertex {
	if _, ok := g.nhTable[u]; !ok {
		return nil
	}
	if _, ok := g.nhTable[u][v]; !ok {
		return nil
	}
	return g.nhTable[u][v]
}

func (g *IG) Weight(u, v config.Vertex) (ret float64) {
	g.edgelock.RLock()
	defer g.edgelock.RUnlock()
	//defer func() { fmt.Println(u, v, ret) }()
	if u == v {
		return 0
	}
	if _, ok := g.edges[u]; !ok {
		return Infinity
	}
	if _, ok := g.edges[u][v]; !ok {
		return Infinity
	}
	if time.Now().After(g.edges[u][v].time.Add(g.NodeReportTimeout)) {
		return Infinity
	}
	return g.edges[u][v].ping
}

func (g *IG) OldWeight(u, v config.Vertex) (ret float64) {
	g.edgelock.RLock()
	defer g.edgelock.RUnlock()
	if u == v {
		return 0
	}
	if _, ok := g.edges[u]; !ok {
		return Infinity
	}
	if _, ok := g.edges[u][v]; !ok {
		return Infinity
	}
	return g.edges[u][v].ping_old
}

func (g *IG) ShouldCalculate() bool {
	vert := g.Vertices()
	for u, _ := range vert {
		for v, _ := range vert {
			if u != v {
				w := g.Weight(u, v)
				if g.ShouldUpdate(u, v, w) {
					return true
				}
			}
		}
	}
	return false
}

func (g *IG) SetWeight(u, v config.Vertex, weight float64) {
	g.edgelock.Lock()
	defer g.edgelock.Unlock()
	if _, ok := g.edges[u]; !ok {
		return
	}
	if _, ok := g.edges[u][v]; !ok {
		return
	}
	g.edges[u][v].ping = weight
}

func (g *IG) SetOldWeight(u, v config.Vertex, weight float64) {
	g.edgelock.Lock()
	defer g.edgelock.Unlock()
	if _, ok := g.edges[u]; !ok {
		return
	}
	if _, ok := g.edges[u][v]; !ok {
		return
	}
	g.edges[u][v].ping_old = weight
}

func (g *IG) RemoveAllNegativeValue() {
	vert := g.Vertices()
	for u, _ := range vert {
		for v, _ := range vert {
			if g.Weight(u, v) < 0 {
				if g.loglevel.LogInternal {
					fmt.Printf("Internal: Remove negative value : edge[%v][%v] = 0\n", u, v)
				}
				g.SetWeight(u, v, 0)
			}
		}
	}
}

func (g *IG) FloydWarshall(again bool) (dist config.DistTable, next config.NextHopTable, err error) {
	if g.loglevel.LogInternal {
		if !again {
			fmt.Println("Internal: Start Floyd Warshall algorithm")
		} else {
			fmt.Println("Internal: Start Floyd Warshall algorithm again")

		}
	}
	vert := g.Vertices()
	dist = make(config.DistTable)
	next = make(config.NextHopTable)
	for u, _ := range vert {
		dist[u] = make(map[config.Vertex]float64)
		next[u] = make(map[config.Vertex]*config.Vertex)
		for v, _ := range vert {
			dist[u][v] = Infinity
		}
		dist[u][u] = 0
		for _, v := range g.Neighbors(u) {
			w := g.Weight(u, v)
			if w < Infinity {
				v := v
				dist[u][v] = w
				next[u][v] = &v
			}
			g.SetOldWeight(u, v, w)
		}
	}
	for k, _ := range vert {
		for i, _ := range vert {
			for j, _ := range vert {
				if dist[i][k] < Infinity && dist[k][j] < Infinity {
					if dist[i][j] > dist[i][k]+dist[k][j] {
						dist[i][j] = dist[i][k] + dist[k][j]
						next[i][j] = next[i][k]
					}
				}
			}
		}
	}
	for i := range dist {
		if dist[i][i] < 0 {
			if !again {
				if g.loglevel.LogInternal {
					fmt.Println("Internal: Error: Negative cycle detected")
				}
				g.RemoveAllNegativeValue()
				err = errors.New("negative cycle detected")
				dist, next, _ = g.FloydWarshall(true)
				return
			} else {
				dist = make(config.DistTable)
				next = make(config.NextHopTable)
				err = errors.New("negative cycle detected again!")
				if g.loglevel.LogInternal {
					fmt.Println("Internal: Error: Negative cycle detected again")
				}
				return
			}
		}
	}
	return
}

func Path(u, v config.Vertex, next config.NextHopTable) (path []config.Vertex) {
	if next[u][v] == nil {
		return []config.Vertex{}
	}
	path = []config.Vertex{u}
	for u != v {
		u = *next[u][v]
		path = append(path, u)
	}
	return path
}

func (g *IG) SetNHTable(nh config.NextHopTable, table_hash [32]byte) { // set nhTable from supernode
	g.nhTable = nh
	g.NhTableHash = table_hash
	g.NhTableExpire = time.Now().Add(g.SuperNodeInfoTimeout)
}

func (g *IG) GetNHTable(recalculate bool) config.NextHopTable {
	if recalculate && time.Now().After(g.NhTableExpire) {
		g.RecalculateNhTable(false)
	}
	return g.nhTable
}

func (g *IG) GetDtst() config.DistTable {
	return g.dlTable
}

func (g *IG) GetEdges(isOld bool) (edges map[config.Vertex]map[config.Vertex]float64) {
	vert := g.Vertices()
	edges = make(map[config.Vertex]map[config.Vertex]float64, len(vert))
	for src, _ := range vert {
		edges[src] = make(map[config.Vertex]float64, len(vert))
		for dst, _ := range vert {
			if src != dst {
				if isOld {
					edges[src][dst] = g.OldWeight(src, dst)
				} else {
					edges[src][dst] = g.Weight(src, dst)
				}
			}
		}
	}
	return
}

func (g *IG) GetBoardcastList(id config.Vertex) (tosend map[config.Vertex]bool) {
	tosend = make(map[config.Vertex]bool)
	for _, element := range g.nhTable[id] {
		tosend[*element] = true
	}
	return
}

func (g *IG) GetBoardcastThroughList(self_id config.Vertex, in_id config.Vertex, src_id config.Vertex) (tosend map[config.Vertex]bool) {
	tosend = make(map[config.Vertex]bool)
	for check_id, _ := range g.GetBoardcastList(self_id) {
		for _, path_node := range Path(src_id, check_id, g.nhTable) {
			if path_node == self_id && check_id != in_id {
				tosend[check_id] = true
				continue
			}
		}
	}
	return
}

func printExample() {
	fmt.Println(`X 1   2   3   4   5   6
1 0   0.5 Inf Inf Inf Inf
2 0.5 0   0.5 0.5 Inf Inf
3 Inf 0.5 0   0.5 0.5 Inf
4 Inf 0.5 0.5 0   Inf 0.5
5 Inf Inf 0.5 Inf 0   Inf
6 Inf Inf Inf 0.5 Inf 0`)
}

func a2n(s string) (ret float64) {
	if s == "Inf" {
		return Infinity
	}
	ret, err := strconv.ParseFloat(s, 64)
	if err != nil {
		panic(err)
	}
	return
}

func a2v(s string) config.Vertex {
	ret, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		panic(err)
	}
	return config.Vertex(ret)
}

func Solve(filePath string, pe bool) error {
	if pe {
		printExample()
		return nil
	}

	g := NewGraph(3, false, config.GraphRecalculateSetting{
		NodeReportTimeout: 9999,
	}, config.NTPinfo{}, config.LoggerInfo{LogInternal: true})
	inputb, err := ioutil.ReadFile(filePath)
	if err != nil {
		return err
	}

	input := string(inputb)
	lines := strings.Split(input, "\n")
	verts := strings.Fields(lines[0])
	for _, line := range lines[1:] {
		element := strings.Fields(line)
		src := a2v(element[0])
		for index, sval := range element[1:] {
			val := a2n(sval)
			dst := a2v(verts[index+1])
			if src != dst && val != Infinity {
				g.UpdateLatency(src, dst, S2TD(val), false, false)
			}
		}
	}
	dist, next, err := g.FloydWarshall(false)
	if err != nil {
		fmt.Println("Error:", err)
	}

	rr, _ := yaml.Marshal(Fullroute{
		Dist: dist,
		Next: next,
	})
	fmt.Print(string(rr))

	fmt.Println("\nHuman readable:")
	fmt.Println("src\tdist\t\tpath")
	for _, U := range verts[1:] {
		u := a2v(U)
		for _, V := range verts[1:] {
			v := a2v(V)
			if u != v {
				fmt.Printf("%d -> %d\t%3f\t%s\n", u, v, dist[u][v], fmt.Sprint(Path(u, v, next)))
			}
		}
	}
	return nil
}
