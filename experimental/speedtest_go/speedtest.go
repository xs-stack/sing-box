package speedtest_go

import (
	"context"
	"errors"
	"fmt"
	"github.com/sagernet/sing-box/experimental/speedtest_go/speedtest"
	"github.com/sagernet/sing-box/experimental/speedtest_go/speedtest/transport"
	"gopkg.in/alecthomas/kingpin.v2"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ShowList      = kingpin.Flag("list", "Show available speedtest.net servers.").Short('l').Bool()
	ServerIds     = kingpin.Flag("server", "Select server id to run speedtest.").Short('s').Ints()
	CustomURL     = kingpin.Flag("custom-url", "Specify the url of the server instead of fetching from speedtest.net.").String()
	SavingMode    = kingpin.Flag("saving-mode", "Test with few resources, though low accuracy (especially > 30Mbps).").Bool()
	JsonOutput    = kingpin.Flag("json", "Output results in json format.").Bool()
	UnixOutput    = kingpin.Flag("unix", "Output results in unix like format.").Bool()
	Location      = kingpin.Flag("location", "Change the location with a precise coordinate (format: lat,lon).").String()
	City          = kingpin.Flag("city", "Change the location with a predefined city label.").String()
	ShowCityList  = kingpin.Flag("city-list", "List all predefined city labels.").Bool()
	Proxy         = kingpin.Flag("proxy", "Set a Proxy(http[s] or socks) for the speedtest.").String()
	Source        = kingpin.Flag("source", "Bind a Source interface for the speedtest.").String()
	DnsBindSource = kingpin.Flag("dns-bind-Source", "DNS request binding Source (experimental).").Bool()
	Multi         = kingpin.Flag("multi", "Enable Multi-server mode.").Short('m').Bool()
	Thread        = kingpin.Flag("thread", "Set the number of concurrent connections.").Short('t').Int()
	Search        = kingpin.Flag("search", "Fuzzy Search servers by a keyword.").String()
	UserAgent     = kingpin.Flag("ua", "Set the user-agent header for the speedtest.").String()
	NoDownload    = kingpin.Flag("no-download", "Disable download test.").Bool()
	NoUpload      = kingpin.Flag("no-upload", "Disable upload test.").Bool()
	NoPacketLoss  = kingpin.Flag("no-packet-loss", "Disable packet loss test.").Bool()
	PingMode      = kingpin.Flag("ping-mode", "Select a method for Ping (support icmp/tcp/http).").Default("http").String()
	Unit          = kingpin.Flag("unit", "Set human-readable and auto-scaled rate units for output (options: decimal-bits/decimal-bytes/binary-bits/binary-bytes).").Short('u').String()
	Debug         = kingpin.Flag("debug", "Enable Debug mode.").Short('d').Bool()
)

var (
	commit = "dev"
	date   = "unknown"
)

func GetNoUploadNoDownload() string {
	*NoDownload = true
	*NoUpload = true
	*NoPacketLoss = true
	*JsonOutput = true
	return do()
}
func main() {
	kingpin.Version(fmt.Sprintf("speedtest-go v%s git-%s built at %s", speedtest.Version(), commit, date))
	kingpin.Parse()
	do()
}

func do() string {

	AppInfo()

	speedtest.SetUnit(parseUnit(*Unit))

	// discard standard log.
	log.SetOutput(io.Discard)

	// start unix output for saving mode by default.
	if *SavingMode && !*JsonOutput && !*UnixOutput {
		*UnixOutput = true
	}

	// 0. speed test setting
	var speedtestClient = speedtest.New(speedtest.WithUserConfig(
		&speedtest.UserConfig{
			UserAgent:      *UserAgent,
			Proxy:          *Proxy,
			Source:         *Source,
			DnsBindSource:  *DnsBindSource,
			Debug:          *Debug,
			PingMode:       parseProto(*PingMode), // TCP as default
			SavingMode:     *SavingMode,
			MaxConnections: *Thread,
			CityFlag:       *City,
			LocationFlag:   *Location,
			Keyword:        *Search,
		}))

	if *ShowCityList {
		speedtest.PrintCityList()
		return ""
	}

	// 1. retrieving user information
	taskManager := InitTaskManager(*JsonOutput, *UnixOutput)
	taskManager.AsyncRun("Retrieving User Information", func(task *Task) {
		u, err := speedtestClient.FetchUserInfo()
		task.CheckError(err)
		task.Printf("ISP: %s", u.String())
		task.Complete()
	})

	// 2. retrieving servers
	var err error
	var servers speedtest.Servers
	var targets speedtest.Servers
	taskManager.Run("Retrieving Servers", func(task *Task) {
		if len(*CustomURL) > 0 {
			var target *speedtest.Server
			target, err = speedtestClient.CustomServer(*CustomURL)
			task.CheckError(err)
			targets = []*speedtest.Server{target}
			task.Println("Skip: Using Custom Server")
		} else if len(*ServerIds) > 0 {
			// TODO: need async fetch to speedup
			for _, id := range *ServerIds {
				serverPtr, errFetch := speedtestClient.FetchServerByID(strconv.Itoa(id))
				if errFetch != nil {
					continue // Silently Skip all ids that actually don't exist.
				}
				targets = append(targets, serverPtr)
			}
			task.CheckError(err)
			task.Printf("Found %d Specified Public Server(s)", len(targets))
		} else {
			servers, err = speedtestClient.FetchServers()
			task.CheckError(err)
			task.Printf("Found %d Public Servers", len(servers))
			if *ShowList {
				task.Complete()
				task.manager.Reset()
				showServerList(servers)
				os.Exit(0)
			}
			targets, err = servers.FindServer(*ServerIds)
			task.CheckError(err)
		}
		task.Complete()
	})
	taskManager.Reset()

	// 3. test each selected server with ping, download and upload.
	for _, server := range targets {
		if !*JsonOutput {
			fmt.Println()
		}
		taskManager.Println("Test Server: " + server.String())
		taskManager.Run("Latency: --", func(task *Task) {
			task.CheckError(server.PingTest(func(latency time.Duration) {
				task.Updatef("Latency: %v", latency)
			}))
			task.Printf("Latency: %v Jitter: %v Min: %v Max: %v", server.Latency, server.Jitter, server.MinLatency, server.MaxLatency)
			task.Complete()
		})

		// 3.0 create a packet loss analyzer, use default options
		analyzer := speedtest.NewPacketLossAnalyzer(&speedtest.PacketLossAnalyzerOptions{
			SourceInterface: *Source,
		})

		blocker := sync.WaitGroup{}
		packetLossAnalyzerCtx, packetLossAnalyzerCancel := context.WithTimeout(context.Background(), time.Second*40)
		taskManager.Run("Packet Loss Analyzer", func(task *Task) {
			blocker.Add(1)
			go func() {
				defer blocker.Done()
				err = analyzer.RunWithContext(packetLossAnalyzerCtx, server.Host, func(packetLoss *transport.PLoss) {
					server.PacketLoss = *packetLoss
				})
				if errors.Is(err, transport.ErrUnsupported) {
					packetLossAnalyzerCancel() // cancel early
				}
			}()
			task.Println("Packet Loss Analyzer: Running in background (<= 30 Secs)")
			task.Complete()
		})

		// 3.1 create accompany Echo
		accEcho := newAccompanyEcho(server, time.Millisecond*500)
		taskManager.RunWithTrigger(!*NoDownload, "Download", func(task *Task) {
			accEcho.Run()
			speedtestClient.SetCallbackDownload(func(downRate speedtest.ByteRate) {
				lc := accEcho.CurrentLatency()
				if lc == 0 {
					task.Updatef("Download: %s (Latency: --)", downRate)
				} else {
					task.Updatef("Download: %s (Latency: %dms)", downRate, lc/1000000)
				}
			})
			if *Multi {
				task.CheckError(server.MultiDownloadTestContext(context.Background(), servers))
			} else {
				task.CheckError(server.DownloadTest())
			}
			accEcho.Stop()
			mean, _, std, minL, maxL := speedtest.StandardDeviation(accEcho.Latencies())
			task.Printf("Download: %s (Used: %.2fMB) (Latency: %dms Jitter: %dms Min: %dms Max: %dms)", server.DLSpeed, float64(server.Context.Manager.GetTotalDownload())/1000/1000, mean/1000000, std/1000000, minL/1000000, maxL/1000000)
			task.Complete()
		})

		taskManager.RunWithTrigger(!*NoUpload, "Upload", func(task *Task) {
			accEcho.Run()
			speedtestClient.SetCallbackUpload(func(upRate speedtest.ByteRate) {
				lc := accEcho.CurrentLatency()
				if lc == 0 {
					task.Updatef("Upload: %s (Latency: --)", upRate)
				} else {
					task.Updatef("Upload: %s (Latency: %dms)", upRate, lc/1000000)
				}
			})
			if *Multi {
				task.CheckError(server.MultiUploadTestContext(context.Background(), servers))
			} else {
				task.CheckError(server.UploadTest())
			}
			accEcho.Stop()
			mean, _, std, minL, maxL := speedtest.StandardDeviation(accEcho.Latencies())
			task.Printf("Upload: %s (Used: %.2fMB) (Latency: %dms Jitter: %dms Min: %dms Max: %dms)", server.ULSpeed, float64(server.Context.Manager.GetTotalUpload())/1000/1000, mean/1000000, std/1000000, minL/1000000, maxL/1000000)
			task.Complete()
		})

		if !*NoPacketLoss {
			time.Sleep(time.Second * 30)
		}
		packetLossAnalyzerCancel()
		blocker.Wait()
		if !*JsonOutput {
			taskManager.Println(server.PacketLoss.String())
		}
		taskManager.Reset()
		speedtestClient.Manager.Reset()
	}
	taskManager.Stop()

	if *JsonOutput {
		json, errMarshal := speedtestClient.JSON(targets)
		if errMarshal != nil {
			panic(errMarshal)
		}
		fmt.Print(string(json))
		return string(json)
	}
	return ""
}

type AccompanyEcho struct {
	stopEcho       chan bool
	server         *speedtest.Server
	currentLatency int64
	interval       time.Duration
	latencies      []int64
}

func newAccompanyEcho(server *speedtest.Server, interval time.Duration) *AccompanyEcho {
	return &AccompanyEcho{
		server:   server,
		interval: interval,
		stopEcho: make(chan bool),
	}
}

func (ae *AccompanyEcho) Run() {
	ae.latencies = make([]int64, 0)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case <-ae.stopEcho:
				cancel()
				return
			default:
				latency, _ := ae.server.HTTPPing(ctx, 1, ae.interval, nil)
				if len(latency) > 0 {
					atomic.StoreInt64(&ae.currentLatency, latency[0])
					ae.latencies = append(ae.latencies, latency[0])
				}
			}
		}
	}()
}

func (ae *AccompanyEcho) Stop() {
	ae.stopEcho <- false
}

func (ae *AccompanyEcho) CurrentLatency() int64 {
	return atomic.LoadInt64(&ae.currentLatency)
}

func (ae *AccompanyEcho) Latencies() []int64 {
	return ae.latencies
}

func showServerList(servers speedtest.Servers) {
	for _, s := range servers {
		fmt.Printf("[%5s] %9.2fkm ", s.ID, s.Distance)

		if s.Latency == -1 {
			fmt.Printf("%v", "Timeout ")
		} else {
			fmt.Printf("%-dms ", s.Latency/time.Millisecond)
		}
		fmt.Printf("\t%s (%s) by %s \n", s.Name, s.Country, s.Sponsor)
	}
}

func parseUnit(str string) speedtest.UnitType {
	str = strings.ToLower(str)
	if str == "decimal-bits" {
		return speedtest.UnitTypeDecimalBits
	} else if str == "decimal-bytes" {
		return speedtest.UnitTypeDecimalBytes
	} else if str == "binary-bits" {
		return speedtest.UnitTypeBinaryBits
	} else if str == "binary-bytes" {
		return speedtest.UnitTypeBinaryBytes
	} else {
		return speedtest.UnitTypeDefaultMbps
	}
}

func parseProto(str string) speedtest.Proto {
	str = strings.ToLower(str)
	if str == "icmp" {
		return speedtest.ICMP
	} else if str == "tcp" {
		return speedtest.TCP
	} else {
		return speedtest.HTTP
	}
}

func AppInfo() {
	if !*JsonOutput {
		fmt.Println()
		fmt.Printf("    speedtest-go v%s (git-%s) @showwin\n", speedtest.Version(), commit)
		fmt.Println()
	}
}
