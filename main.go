package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"unsafe"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/ziutek/dvb/linuxdvb/frontend"
)

var (
	basePath = flag.String("devpath", "/dev/dvb", "Base path to dvb adapters")
	listen   = flag.String("listen", ":8027", "Listen bind in format [host]:port")
)

type frontendEntry struct {
	ID     int
	Name   string
	Device *frontend.Device
}

type adapterEntry struct {
	ID        int
	Name      string
	Frontends map[string]*frontendEntry
}

var adapters = make(map[string]*adapterEntry)

func formatLabels(labelPairs []string) string {
	l := len(labelPairs)
	if l == 0 || l%2 != 0 {
		return ""
	}

	str := "{"
	for i, it := range labelPairs {
		if i%2 == 0 {
			if i > 0 {
				str += `,`
			}
			str += it + `="`
		} else {
			str += it + `"`
		}
	}
	str += "}"

	return str
}

func valToString(value interface{}) (string, error) {
	switch v := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", v), nil
	case float32, float64:
		return fmt.Sprintf("%f", v), nil
	case bool:
		if v {
			return "1", nil
		}
		return "0", nil
	default:
		return "", fmt.Errorf("unsupported value type: %T", v)
	}
}

func writeSingle(wr io.Writer, typeStr string, name string, val interface{}, help string, labelPairs []string) error {
	header := ""
	if len(help) > 0 {
		header = "# HELP " + name + " " + help + "\n"
	}
	header += "# TYPE " + name + " " + typeStr + "\n"

	// format bool as int
	valStr, err := valToString(val)
	if err != nil {
		return err
	}

	str := fmt.Sprintf("%s%s%s %s\n\n", header, name, formatLabels(labelPairs), valStr)
	_, err = wr.Write([]byte(str))
	return err
}

func mkPairs(m map[string]string) []string {
	var arr []string
	for k, v := range m {
		arr = append(arr, k, v)
	}
	return arr
}

func writeGauge(wr io.Writer, name string, val interface{}, help string, labelPairs []string) error {
	return writeSingle(wr, "gauge", name, val, help, labelPairs)
}

func writeCounter(wr io.Writer, name string, val interface{}, help string, labelPairs []string) error {
	return writeSingle(wr, "counter", name, val, help, labelPairs)
}

func handleMetrics(c echo.Context) error {
	resp := c.Response()
	resp.Header().Set("Content-Type", "text/plain")
	resp.WriteHeader(http.StatusOK)

	labels := make(map[string]string)

	for adapterName, a := range adapters {
		for frontendName, f := range a.Frontends {
			clear(labels)

			labels["adapter"] = strconv.Itoa(a.ID)
			labels["frontend"] = strconv.Itoa(f.ID)

			fe := f.Device
			fe3 := frontend.API3{Device: *fe}

			// stat, err := fe.Stat()
			// if err != nil {
			// 	slog.Error("error getting frontend state", "adapter", adapterName, "frontend", frontendName, "error", err)
			// 	continue
			// }

			// deadline := time.Now().Add(1 * time.Second)

			// var ev frontend.Event
			// timedout, err := fe3.WaitEvent(&ev, deadline)
			// if timedout {
			// 	continue
			// }
			// if err != nil {
			// 	slog.Error("error waiting for frontend event", "adapter", adapterName, "frontend", frontendName, "error", err)
			// 	continue
			// }
			// st := ev.Status()

			st, err := fe3.Status()
			if err != nil {
				slog.Error("error getting fe status", "adapter", adapterName, "frontend", frontendName, "error", err)
				continue
			}

			writeGauge(resp.Writer, "dvb_fe_has_signal", st&frontend.HasSignal > 0, "Frontend found something above the noise levell", mkPairs(labels))
			writeGauge(resp.Writer, "dvb_fe_has_carrier", st&frontend.HasCarrier > 0, "Frontend found a DVB signal", mkPairs(labels))
			writeGauge(resp.Writer, "dvb_fe_has_viterbi", st&frontend.HasViterbi > 0, "FEC is stable", mkPairs(labels))
			writeGauge(resp.Writer, "dvb_fe_has_sync", st&frontend.HasSync > 0, "Frontend found sync bytes", mkPairs(labels))
			writeGauge(resp.Writer, "dvb_fe_has_lock", st&frontend.HasLock > 0, "Frontend is receiving data", mkPairs(labels))

			ber, err := fe3.BER()
			if err == nil {
				writeGauge(resp.Writer, "dvb_fe_ber", ber, "Bit error rate for the signal currently received/demodulated", mkPairs(labels))
			}

			snr, err := fe3.SNR()
			if err == nil {
				snrPerc := int(*(*uint16)(unsafe.Pointer(&snr))) * 100 / 0xffff
				writeGauge(resp.Writer, "dvb_fe_snr_percent", snrPerc, "Signal-to-noise ratio for the signal currently received by the front-end", mkPairs(labels))
			}

			ss, err := fe3.SignalStrength()
			if err == nil {
				ssPerc := int(*(*uint16)(unsafe.Pointer(&ss))) * 100 / 0xffff
				writeGauge(resp.Writer, "dvb_fe_signal_strength_percent", ssPerc, "Signal strength value for the signal currently received by the front-end", mkPairs(labels))
			}

			ub, err := fe3.UncorrectedBlocks()
			if err == nil {
				writeCounter(resp.Writer, "dvb_fe_uncorrected_blocks_total", ub, "Number of uncorrected blocks detected by the device driver during its lifetime", mkPairs(labels))
			}

		}
	}

	return nil
}

func main() {
	flag.Parse()

	// Check if the base path exists
	if _, err := os.Stat(*basePath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Base path %s does not exist\n", *basePath)
		os.Exit(1)
	}

	// Walk through the base path to find adapters
	adapterPaths, err := filepath.Glob(filepath.Join(*basePath, "adapter*"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error finding adapters: %v\n", err)
		return
	}

	for _, adapterName := range adapterPaths {
		// Extract adapter number
		adapterNumStr := filepath.Base(adapterName)[7:] // "adapter" is 7 characters long
		adapterNum, err := strconv.Atoi(adapterNumStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid adapter number: %s\n", adapterNumStr)
			continue
		}

		// Walk through each adapter to find frontends
		frontendPaths, err := filepath.Glob(filepath.Join(adapterName, "frontend*"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error finding frontends in %s: %v\n", adapterName, err)
			continue
		}

		for _, frontendName := range frontendPaths {
			// Extract frontend number
			frontendNumStr := filepath.Base(frontendName)[8:] // "frontend" is 8 characters long
			frontendNum, err := strconv.Atoi(frontendNumStr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Invalid frontend number: %s\n", frontendNumStr)
				continue
			}

			// Process the frontend
			slog.Info("Found a device", "adapter", adapterNum, "frontend", frontendNum)

			fpath := filepath.Join(*basePath, "adapter"+strconv.Itoa(adapterNum), "frontend"+strconv.Itoa(frontendNum))
			fdev, err := frontend.OpenRO(fpath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error opening frontend %s: %v\n", fpath, err)
				os.Exit(2)
			}

			a := adapters[adapterName]
			if a == nil {
				a = &adapterEntry{
					ID:        adapterNum,
					Name:      adapterName,
					Frontends: make(map[string]*frontendEntry),
				}
				adapters[adapterName] = a
			}

			f := a.Frontends[frontendName]
			if f == nil {
				f = &frontendEntry{
					ID:     frontendNum,
					Name:   frontendName,
					Device: &fdev,
				}
				a.Frontends[frontendName] = f
			}
		}
	}

	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.Recover())

	e.GET("/metrics", handleMetrics)
	e.GET("/", func(c echo.Context) error { return c.Redirect(http.StatusTemporaryRedirect, "/metrics") })

	fmt.Fprintf(os.Stderr, "Error listening: %v", e.Start(*listen))
}
