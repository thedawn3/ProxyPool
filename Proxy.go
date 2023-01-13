package ProxyPool

// test1
import (
	"fmt"
	"github.com/go-redis/redis"
	"github.com/imroc/req/v3"
	"github.com/spf13/viper"
	"github.com/tidwall/gjson"
	"log"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var mu = sync.Mutex{}
var rdb *redis.Client
var c config

type config struct {
	ProxyConfig proxyConfig
	RedisConfig redis.Options
}
type proxyConfig struct {
	ApiUrl          string
	MinSetProxyTime int64
	MinProxyNum     int64
	ProxyTimeout    time.Duration
	ProxyRepetition bool
}

type ProxyInfo struct {
	Proxy      string `json:"Proxy"`
	ExpireTime int64  `json:"ExpireTime"`
}

// GetProxy 从库中获取一条代理信息,剩余时间不低于d秒 失败返回nil
func GetProxy(d int) (Proxy *ProxyInfo) {
	var Gb bool
	Proxy = new(ProxyInfo)
	l := rdb.LLen("okproxy").Val()
	if l == 0 {
		Gb = true
		if mu.TryLock() {
			joinProxy()
			l = rdb.LLen("okproxy").Val()
		} else {
			for i := 0; i < 10; i++ {
				l = rdb.LLen("okproxy").Val()
				if l == 0 {
					time.Sleep(1 * time.Second)
					continue
				} else {
					break
				}
			}
		}
	} else if l < c.ProxyConfig.MinProxyNum {
		Gb = true
		if mu.TryLock() {
			joinProxy()
		}
	}
	for i := 0; i < int(l); i++ {
		res, err := rdb.BLPop(10*time.Second, "okproxy").Result()
		if err != nil {
			fmt.Println(err)
			return nil
		} else {
			str := res[1]
			json := gjson.Parse(str)
			Proxy.Proxy = json.Get("Proxy").String()
			Proxy.ExpireTime = json.Get("expireTime").Int()
			if int(Proxy.ProxyLTime()) < d {
				continue
			}
			return Proxy
		}
	}
	if !Gb {
		return GetProxy(d)
	}
	return nil
}

// ProxyLTime 返回代理剩余时间
func (P *ProxyInfo) ProxyLTime() (TTL int64) {
	unix := time.Now().Unix()
	TTL = P.ExpireTime - unix
	return TTL
}

// ProxySetEx 自压入代理 到list尾部
func (P *ProxyInfo) ProxySetEx() {
	JsonStr := fmt.Sprintf(`{"Proxy":"%v","expireTime":%v}`, P.Proxy, P.ExpireTime)
	rdb.LPush("okproxy", JsonStr)
}

// ProxyPing 测试代理是否超时
func (P *ProxyInfo) ProxyPing() bool {
	ProxyURL := "http://" + P.Proxy
	Client := req.C().SetProxyURL(ProxyURL).SetTimeout(c.ProxyConfig.ProxyTimeout)
	resp, err := Client.R().Get("http://www.soso.com/")
	if err != nil {
		return false
	}
	if strings.Contains(resp.String(), "soso") == true {
		return true
	}
	return false
}

func init() {
	viper.SetConfigName("ProxyConfig")
	viper.AddConfigPath("./Config/")
	viper.SetConfigType("yaml")
	err := viper.ReadInConfig()
	if err != nil {
		log.Fatal("代理配置加载失败", err.Error())
	}
	err = viper.Unmarshal(&c)
	if err != nil {
		log.Fatal("代理配置加载失败", err.Error())
	}
	//fmt.Println(c)
	proxyRedis()
	ok, str := testGetProxy()
	if !ok {
		log.Fatal("代理提取失败", str)
	}
}

// 初始化redis连接
func proxyRedis() {
	rdb = redis.NewClient(&c.RedisConfig)
	_, err := rdb.Ping().Result()
	if err != nil {
		log.Fatal(err)
	}
	//fmt.Println("redis连接成功！", rdb)
}

// 校验入库全流程
func joinProxy() {
	defer mu.Unlock()
	var p float64
	var okp int32
	var wg sync.WaitGroup
	timeLayout := "2006-01-02 15:04:05"

	s, ok := proxyApi()
	if !ok {
		return
	}
	result := gjson.Parse(s)
	success := result.Get("success").Bool()
	if success {
		p = result.Get("data.#").Num
		if p == 0 {
			fmt.Println("请求代理API后未取得代理")
			return
		}
		result.Get("data").ForEach(func(key, value gjson.Result) bool {
			ip := gjson.Get(value.String(), "ip").String()
			port := gjson.Get(value.String(), "port").Int()
			expireTime := gjson.Get(value.String(), "expire_time").String()
			loc, _ := time.LoadLocation("Local")
			times, _ := time.ParseInLocation(timeLayout, expireTime, loc)
			Proxy := ProxyInfo{
				Proxy:      fmt.Sprint(ip, ":", port),
				ExpireTime: times.Unix(),
			}
			wg.Add(1)
			go Proxy.proxySet(&wg, &okp)
			return true // keep iterating

		})
	} else {
		fmt.Printf(s)
	}
	wg.Wait()
	fmt.Printf("[%s] 获取到%v条Proxy,成功验证入库%v条Proxy \n", time.Now().Format("15:04:05"), p, okp)
}

// 验证代理入库流程
func (P *ProxyInfo) proxySet(wg *sync.WaitGroup, okp *int32) {
	defer wg.Done()
	TTL := P.ProxyLTime()
	if TTL < c.ProxyConfig.MinSetProxyTime {
		return
	}
	KEY := fmt.Sprint("proxy:", strings.ReplaceAll(P.Proxy, ":", "@"))
	if c.ProxyConfig.ProxyRepetition {
		ok := rdb.SetNX(KEY, "", time.Duration(TTL)*time.Second).Val()
		if !ok {
			return
		}
	}
	if P.ProxyPing() {
		atomic.AddInt32(okp, 1)
		JsonStr := fmt.Sprintf(`{"Proxy":"%v","expireTime":%v}`, P.Proxy, P.ExpireTime)
		rdb.RPush("okproxy", JsonStr)
	}
}
func testGetProxy() (bool, string) {
	re := regexp.MustCompile(`count=\d+`)
	url := re.ReplaceAllString(c.ProxyConfig.ApiUrl, "count=1")
	resp, err := req.C().SetTimeout(5 * time.Second).
		R().Get(url)
	if err != nil {
		fmt.Println(err.Error())
		return false, err.Error()
	}
	if !gjson.Get(resp.String(), "success").Bool() {
		return false, resp.String()
	}
	return true, ""
}

// 从代理API处获取n条代理
func proxyApi() (string, bool) {
	resp, err := req.C().SetTimeout(5 * time.Second).
		R().Get(c.ProxyConfig.ApiUrl)
	if err != nil {
		fmt.Println(err.Error())
		return resp.Err.Error(), false
	}
	return resp.String(), true
}
