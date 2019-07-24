package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	_ "github.com/mattn/go-sqlite3"
	"github.com/tarm/serial"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

const RADAR_HISTORY_LENGTH = 36

var radarHistory [][]byte
var dbMutex sync.Mutex
var tzone string

func getDB() (*sql.DB, error) {
	return sql.Open("sqlite3", "./lightning.db")
}

func addLabel(img *image.NRGBA, x, y int, label string) {
	col := color.RGBA{200, 100, 0, 255}
	point := fixed.Point26_6{fixed.Int26_6(x * 64), fixed.Int26_6(y * 64)}

	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(col),
		Face: basicfont.Face7x13,
		Dot:  point,
	}
	d.DrawString(label)
}

func colorDiff(a color.Color, b color.Color) float64 {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	dr := int(ar) - int(br)
	dg := int(ag) - int(bg)
	db := int(ab) - int(bb)
	return math.Sqrt(float64(dr*dr + dg*dg + db*db))
}

var radarPos = 0

func getRadarImage() {
	radarPos = 0
	for {
		log.Println("Loading radar image...")
		resp, err := http.Get("https://radar.weather.gov/ridge/Conus/RadarImg/latest_radaronly.gif")
		if err != nil {
			log.Fatalln(err)
		}
		i, _ := imaging.Decode(resp.Body)

		// Reproject from the NOAA EPSG:4326 to the OSM EPSG:3857
		var yy, yd float64
		ni := imaging.New(i.Bounds().Dx(), i.Bounds().Dy(), color.NRGBA{0, 0, 0, 0})
		for x := 0; x < ni.Bounds().Dx(); x++ {
			yy = 0
			lasty := 0
			yd = 1.2
			for y := 0; y < ni.Bounds().Dy(); y++ {
				ny := int(math.Floor(yy))
				c := i.At(x, y)
				d := colorDiff(c, color.RGBA{0, 255, 255, 255})
				if d > 60000.0 {
					ni.Set(x, ny, c)
					if (ny - lasty) > 1 {
						ni.Set(x, ny-1, c)
					}
				}
				yy += yd
				yd -= 0.00025
				lasty = ny
			}
		}
		addLabel(ni, 10, 10, time.Now().String())
		buffer := new(bytes.Buffer)
		if err := png.Encode(buffer, ni); err != nil {
			log.Println("unable to encode image.")
		}

		radarHistory[radarPos] = buffer.Bytes()
		now := time.Now()
		min := (int(math.Floor(float64(now.Unix())/600)) % 6) * 10
		fn := fmt.Sprintf("./radar_%s%02d.png", now.Format("2006010215"), min)
		log.Printf("Saving radar image %s\n", fn)
		out, err := os.Create(fn)
		if err != nil {
			fmt.Println(err)
		} else {
			buffer.WriteTo(out)
			out.Close()
		}

		// Remove older radar image
		now = now.Add(RADAR_HISTORY_LENGTH * -10 * time.Minute)
		min = (int(math.Floor(float64(now.Unix())/600)) % 6) * 10
		fn = fmt.Sprintf("./radar_%s%02d.png", now.Format("2006010215"), min)
		log.Printf("Removing file %s\n", fn)
		os.Remove(fn)

		radarPos++
		if radarPos >= RADAR_HISTORY_LENGTH {
			radarPos = 0
		}
		time.Sleep(601 * time.Second)
	}
}

func sendRadar(w http.ResponseWriter, p int) {
	pos := radarPos - 1 - p
	if pos < 0 {
		pos += RADAR_HISTORY_LENGTH
	}
	if pos >= RADAR_HISTORY_LENGTH {
		pos -= RADAR_HISTORY_LENGTH
	}
	b := radarHistory[pos]
	if len(b) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-type", "image/gif")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if _, err := w.Write(b); err != nil {
		log.Println("unable to write image.")
	}
}

func checksum(in string) string {
	sum := 0
	for i := 1; i < len(in); i++ {
		sum ^= (int)(in[i])
	}
	return fmt.Sprintf("%02X", sum)
}

func apiGetStrikes(w http.ResponseWriter) {
	type STRIKE struct {
		Timestamp string  `json:"ts"`
		Age       int64   `json:"age"`
		Dist      float64 `json:"dmeters"`
		Hdg       float64 `json:"hdg"`
	}
	type DAILY struct {
		Day string   `json:"day"`
		Cnt [3]int64 `json:"cnt"`
	}
	type DATA struct {
		Strike []STRIKE
		Hourly [][]int64
		Daily  []DAILY
	}
	var data DATA
	data.Strike = make([]STRIKE, 0)
	data.Hourly = make([][]int64, 72)
	data.Daily = make([]DAILY, 31)

	now := time.Now()
	earliest := now.Add(-1 * time.Hour).Format("20060102150405")

	dbMutex.Lock()
	defer dbMutex.Unlock()
	db, _ := getDB()
	rows, _ := db.Query(`SELECT *
														FROM strikes
														WHERE ts >= '` + earliest + `'
														ORDER BY ts DESC`)
	for rows.Next() {
		var ts string
		var dist, hdg float64

		rows.Scan(&ts, &dist, &hdg)

		tts, _ := time.Parse("20060102150405 MST", ts+" "+tzone)
		tts = tts.Local()
		dist *= 1609 // Meters
		hdg *= math.Pi / 180.0
		age := int64(time.Now().UTC().Sub(tts).Seconds())
		data.Strike = append(data.Strike, STRIKE{Timestamp: ts, Age: age, Dist: dist, Hdg: hdg})
	}
	rows.Close()

	earliest = now.Add(-72 * time.Hour).Format("20060102150405")
	rows, _ = db.Query(`SELECT SUBSTR(ts,1,10) AS hour, (distance/100) AS dist, COUNT(1) AS cnt
														FROM strikes
														WHERE ts >= '` + earliest + `'
														GROUP BY hour, dist
														ORDER BY dist, hour DESC`)
	for rows.Next() {
		var ts string
		var cnt, dist int64

		rows.Scan(&ts, &dist, &cnt)
		tts, _ := time.Parse("20060102150405 MST", ts+"0000 "+tzone)
		if dist > 2 {
			dist = 2
		}
		age := int64(time.Since(tts).Seconds() / 3600)
		if age >= 72 {
			continue
		}
		if data.Hourly[age] == nil {
			data.Hourly[age] = make([]int64, 3)
		}
		data.Hourly[age][dist] = cnt
	}

	earliest = now.AddDate(0, 0, -31).Format("20060102150405")
	rows, _ = db.Query(`SELECT SUBSTR(ts,1,8) AS day, (distance/100) AS dist, COUNT(1) AS cnt
														FROM strikes
														WHERE ts >= '` + earliest + `'
														GROUP BY day, dist
														ORDER BY dist, day DESC`)
	for rows.Next() {
		var ts string
		var cnt, dist int64

		rows.Scan(&ts, &dist, &cnt)
		tts, _ := time.Parse("20060102150405 MST", ts+"000000 "+tzone)
		if dist > 2 {
			dist = 2
		}
		age := int64(time.Since(tts).Seconds() / 86400)
		if data.Daily[age].Day == "" {
			data.Daily[age] = DAILY{Day: ts}
		}
		data.Daily[age].Cnt[dist] = cnt
	}

	json.NewEncoder(w).Encode(data)
}

func api(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s\n", r.RequestURI)
	apifunc := r.RequestURI[5:]

	if apifunc == "getStrikes" {
		apiGetStrikes(w)
	} else if apifunc[0:8] == "getRadar" {
		amp := strings.Index(apifunc, "&")
		if amp > 0 {
			pos, _ := strconv.Atoi(apifunc[8:amp])
			sendRadar(w, pos)
		} else {
			pos, _ := strconv.Atoi(apifunc[8:])
			sendRadar(w, pos)
		}
	}
	fmt.Printf("apifunc(%s)\n", apifunc)
}

func processLine(line string) {
	if line[0:6] == "$WIMLI" {
		fmt.Printf("Strike %s\n", line)
		var dist, udist int32
		var hdg float64
		fmt.Sscanf(line[7:], "%d,%d,%f", &dist, &udist, &hdg)
		now := time.Now().Format("20060102150405")
		dbMutex.Lock()
		defer dbMutex.Unlock()
		pg, _ := getDB()
		statement, err := pg.Prepare("INSERT INTO strikes (ts, distance, heading) VALUES(?, ?, ?)")
		if err != nil {
			log.Println(err)
			pg.Close()
			return
		}
		statement.Exec(now, dist, hdg)
		statement.Close()
		pg.Close()
	} else if line[0:6] == "$WIMLN" {
		//fmt.Printf("Noise %s\n", line)
	} else if line[0:6] == "$WIMST" {
		//fmt.Printf("Status %s\n", line)
	}
}

func handleLD250(s *serial.Port) {
	strbuf := ""
	buf := make([]byte, 128)
	for {
		n, err := s.Read(buf)
		if err != nil {
			log.Fatal(err)
		}
		strbuf += string(buf[0:n])
		pos := strings.Index(strbuf, "\n")
		if pos >= 0 {
			line := strings.Trim(strbuf[0:pos], "\r\n")
			if len(line) > 3 {
				//fmt.Printf("LINE(%s)\n", line)
				chksum := checksum(line[0 : len(line)-3])
				if chksum == line[len(line)-2:] {
					processLine(line[0 : len(line)-3])
				} else {
					fmt.Printf("Bad checksum (%s) expecting %s\n", line, chksum)
				}
			}
			strbuf = strbuf[pos+1:]
		}
	}
}

func main() {
	tzone, _ = time.Now().Zone()
	db, _ := getDB()
	db.Exec("CREATE TABLE strikes (ts text, distance integer, heading integer)")
	db.Close()

	// Load radar
	radarHistory = make([][]byte, RADAR_HISTORY_LENGTH)
	radarPos = 0
	now := time.Now()
	for a := 0; a < RADAR_HISTORY_LENGTH; a++ {
		min := (int(math.Floor(float64(now.Unix())/600)) % 6) * 10
		fn := fmt.Sprintf("./radar_%s%02d.png", now.Format("2006010215"), min)
		fmt.Printf("Loading buffer %d with %s\n", radarPos, fn)
		in, err := os.OpenFile(fn, os.O_RDONLY, 0)
		now = now.Add(-10 * time.Minute)
		if err == nil {
			b := new(bytes.Buffer)
			// read into buffer
			b.ReadFrom(in)
			radarHistory[radarPos] = b.Bytes()
		} else {
			fmt.Println("  Cannot load image")
		}
		radarPos--
		if radarPos < 0 {
			radarPos = RADAR_HISTORY_LENGTH - 1
		}
		in.Close()
	}

	c := &serial.Config{Name: "/dev/ttyUSB0", Baud: 9600}
	s, err := serial.OpenPort(c)
	if err != nil {
		log.Fatal(err)
	}
	go handleLD250(s)
	go getRadarImage()

	http.HandleFunc("/api/", api)
	http.Handle("/", http.FileServer(http.Dir("www")))

	origin, _ := url.Parse("http://192.168.1.4:8123/")
	director := func(req *http.Request) {
		req.Header.Add("X-Forwarded-Host", req.Host)
		req.Header.Add("X-Origin-Host", origin.Host)
		req.URL.Scheme = "http"
		req.URL.Host = origin.Host
	}
	proxy := &httputil.ReverseProxy{Director: director}
	proxy.Transport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   120 * time.Second,
			KeepAlive: 60 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	http.HandleFunc("/tile/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("serverProxy %s", r.RequestURI)
		start := time.Now()
		proxy.ServeHTTP(w, r)
		log.Printf("%f done serverProxy %s", time.Since(start).Seconds(), r.RequestURI)
	})

	err = http.ListenAndServeTLS("0.0.0.0:8888", "server.crt", "server.key", nil)
	if err != nil {
		log.Printf("Listen: %v\n", err)
	}
}
