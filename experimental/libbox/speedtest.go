package libbox

import "github.com/sagernet/sing-box/experimental/speedtest_go"

func GetBasicSpeedtestData() string {
	return speedtest_go.GetNoUploadNoDownload()
}
