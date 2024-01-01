package main

import (
	"archive/tar"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/schollz/progressbar/v3"
	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

const defaultReadTimeout = 5 * time.Second

func main() {
	if len(os.Args) != 2 {
		log.Fatalf("Usage: %s <path to os.bin>", os.Args[0])
	}
	customOs := os.Args[1]

	list, err := enumerator.GetDetailedPortsList()
	if err != nil {
		log.Fatalf("Could not get port list: %s", err)
	}
	var details *enumerator.PortDetails
	for _, port := range list {
		if port.IsUSB && port.VID == "3310" && port.PID == "0300" {
			details = port
		}
	}
	if details == nil {
		log.Fatalf("Could not find Harmony device")
	}
	port, err := serial.Open(details.Name, &serial.Mode{
		BaudRate: 1200,
	})
	if err != nil {
		log.Fatalf("Could not open port: %s", err)
	}
	defer func() {
		_ = port.Close()
	}()

	var di deviceInformation
	err = request(port, defaultReadTimeout, map[string]interface{}{
		"endpoint": 1,
		"method":   1,
	}, &di)
	if err != nil {
		log.Fatalf("Could not make request: %s", err)
	}
	diJson, err := json.MarshalIndent(di, "", "    ")
	if err != nil {
		log.Fatalf("Could not marshal JSON: %s", err)
	}
	fmt.Printf("Device information: %s\n", diJson)

	resp, err := http.Get("https://api.center.mudita.com/.netlify/functions/v2-get-release?product=BellHybrid&environment=production&version=latest")
	if err != nil {
		log.Fatalf("Could not get latest release: %s", err)
	}
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("Error getting latest release: %d", resp.StatusCode)
	}

	var r release
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Could not read response body: %s", err)
	}
	err = json.Unmarshal(respBody, &r)
	if err != nil {
		log.Fatalf("Could not parse response body: %s", err)
	}

	if _, err := os.Stat(r.File.Name); errors.Is(err, os.ErrNotExist) {
		fmt.Printf("Downloading update file: %s\n", r.File.Name)
		resp, err := http.Get(r.File.URL)
		if err != nil {
			log.Fatalf("Could not download update file: %s", err)
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		out, err := os.Create(r.File.Name)
		if err != nil {
			log.Fatalf("Could not create file: %s", err)
		}
		defer func() {
			_ = out.Close()
		}()
		bar := progressbar.DefaultBytes(resp.ContentLength, "Downloading")
		_, err = io.Copy(io.MultiWriter(out, bar), resp.Body)
		if err != nil {
			log.Fatalf("Could not download update file: %s", err)
		}
	}
	oldTar, err := os.ReadFile(r.File.Name)
	if err != nil {
		log.Fatalf("Could not read update file: %s", err)
	}

	if _, err := os.Stat(customOs); errors.Is(err, os.ErrNotExist) {
		log.Fatalf("Custom OS file does not exist: %s", customOs)
	}
	customOsBuffer, err := os.ReadFile(customOs)
	if err != nil {
		log.Fatalf("Could not read custom OS file: %s", err)
	}
	fmt.Printf("Patching update file with custom OS: %s\n", customOs)
	var newTar bytes.Buffer
	err = ReplaceOsInTar(bytes.NewBuffer(oldTar), &newTar, customOsBuffer)
	if err != nil {
		log.Fatalf("Could not replace OS in tar: %s", err)
	}
	update := newTar.Bytes()

	if di.OnboardingState != "1" {
		log.Fatalf("Device has not completed onboarding")
	}

	err = request(port, defaultReadTimeout, map[string]interface{}{
		"endpoint": 3,
		"method":   4,
		"body": map[string]interface{}{
			"removeFile": di.UpdateFilePath,
		},
	}, nil)
	if err != nil {
		log.Fatalf("Error deleting update file: %s", err)
	}

	totalSpace, err := strconv.ParseFloat(di.DeviceSpaceTotal, 64)
	if err != nil {
		log.Fatalf("Could not parse device space total: %s", err)
	}
	userSpace, err := strconv.ParseFloat(di.UsedUserSpace, 64)
	if err != nil {
		log.Fatalf("Could not parse used user space: %s", err)
	}
	deviceSpace, err := strconv.ParseFloat(di.SystemReservedSpace, 64)
	if err != nil {
		log.Fatalf("Could not parse system reserved space: %s", err)
	}
	freeSpace := (totalSpace - userSpace - deviceSpace) * 1024 * 1024
	if len(update)*3 > int(freeSpace) {
		log.Fatalf("Not enough free space for update: %d > %d", len(update)*3, int(freeSpace))
	}

	table := crc32.MakeTable(crc32.IEEE)
	crc := crc32.Checksum(update, table)

	var uploadResp fileUpload
	err = request(port, defaultReadTimeout, map[string]interface{}{
		"endpoint": 3,
		"method":   3,
		"body": map[string]interface{}{
			"fileSize":  len(update),
			"fileCrc32": fmt.Sprintf("%08x", crc),
			"fileName":  di.UpdateFilePath,
		},
	}, &uploadResp)
	if err != nil {
		log.Fatalf("Could not make upload request: %s", err)
	}

	bar := progressbar.DefaultBytes(int64(len(update)), "Uploading")
	chunks := (len(update) + uploadResp.ChunkSize - 1) / uploadResp.ChunkSize
	for i := 0; i < chunks; i++ {
		if err != nil {
			log.Fatalf("Error updating progress bar: %s", err)
		}
		end := (i + 1) * uploadResp.ChunkSize
		if end > len(update) {
			end = len(update)
		}
		chunk := update[i*uploadResp.ChunkSize : end]
		err = bar.Add(len(chunk))
		err = request(port, defaultReadTimeout, map[string]interface{}{
			"endpoint": 3,
			"method":   3,
			"body": map[string]interface{}{
				"txID":    uploadResp.TxID,
				"chunkNo": i + 1,
				"data":    chunk,
			},
		}, nil)
		if err != nil {
			log.Fatalf("Error uploading chunk %d: %s", i+1, err)
		}
	}

	// update + reboot
	fmt.Printf("Validating image, please wait 1-2 minutes...\n")
	err = request(port, 10*time.Minute, map[string]interface{}{
		"endpoint": 2,
		"method":   2,
		"body": map[string]interface{}{
			"update": true,
			"reboot": true,
		},
	}, nil)
	if err != nil {
		log.Fatalf("Error updating and rebooting: %s", err)
	}
	fmt.Printf("Rebooting, please wait for your device to update\n")
}

type response struct {
	Body     json.RawMessage `json:"body"`
	Endpoint int             `json:"endpoint"`
	Status   int             `json:"status"`
	Uuid     int             `json:"uuid"`
}

type release struct {
	Version           string   `json:"version"`
	Date              string   `json:"date"`
	Product           string   `json:"product"`
	File              file     `json:"file"`
	MandatoryVersions []string `json:"mandatoryVersions"`
}

type file struct {
	URL  string `json:"url"`
	Size string `json:"size"`
	Name string `json:"name"`
}

type deviceInformation struct {
	BackupFilePath      string `json:"backupFilePath"`
	BatteryLevel        string `json:"batteryLevel"`
	BatteryState        string `json:"batteryState"`
	CaseColour          string `json:"caseColour"`
	CurrentRTCTime      string `json:"currentRTCTime"`
	DeviceSpaceTotal    string `json:"deviceSpaceTotal"`
	GitBranch           string `json:"gitBranch"`
	GitRevision         string `json:"gitRevision"`
	MtpPath             string `json:"mtpPath"`
	OnboardingState     string `json:"onboardingState"`
	RecoveryStatusPath  string `json:"recoveryStatusFilePath"`
	SerialNumber        string `json:"serialNumber"`
	SyncFilePath        string `json:"syncFilePath"`
	SystemReservedSpace string `json:"systemReservedSpace"`
	UpdateFilePath      string `json:"updateFilePath"`
	UsedUserSpace       string `json:"usedUserSpace"`
	Version             string `json:"version"`
}

type fileUpload struct {
	ChunkSize int `json:"chunkSize"`
	TxID      int `json:"txID"`
}

func request(port serial.Port, timeout time.Duration, payload map[string]interface{}, v any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = port.Write([]byte(fmt.Sprintf("#%09d%s", len(buf), buf)))
	if err != nil {
		return err
	}
	buffer := make([]byte, 10)
	_, err = io.ReadFull(port, buffer)
	if err != nil {
		return err
	}
	if string(buffer[:1]) != "#" {
		return fmt.Errorf("invalid response: %s", string(buffer))
	}
	length, err := strconv.ParseInt(string(buffer[1:]), 10, 64)
	if err != nil {
		return err
	}
	buffer = make([]byte, length)
	err = port.SetReadTimeout(timeout)
	if err != nil {
		return err
	}
	_, err = io.ReadFull(port, buffer)
	if err != nil {
		return err
	}

	var r response
	err = json.Unmarshal(buffer, &r)
	if err != nil {
		return err
	}

	if r.Status < 200 || r.Status > 299 {
		return fmt.Errorf("status code %d", r.Status)
	}
	if r.Status == 204 || v == nil {
		return nil
	}

	return json.Unmarshal(r.Body, v)
}

func ReplaceOsInTar(r io.Reader, w io.Writer, osBin []byte) error {
	tr := tar.NewReader(r)
	tw := tar.NewWriter(w)
	defer func() {
		_ = tw.Close()
	}()
	for {
		header, err := tr.Next()
		switch {
		case err == io.EOF:
			return nil
		case err != nil:
			return err
		case header == nil:
			continue
		}

		reader := io.Reader(tr)
		if header.Typeflag == tar.TypeReg {
			if header.Name == "bin/os.bin" {
				reader = bytes.NewReader(osBin)
				header.Size = int64(len(osBin))
			} else if header.Name == "version.json" {
				version := make(map[string]interface{})
				versionOld, err := io.ReadAll(tr)
				if err != nil {
					return err
				}
				err = json.Unmarshal(versionOld, &version)
				if err != nil {
					return err
				}
				osInterface, ok := version["os"]
				if !ok {
					return errors.New("no os in version.json")
				}
				osMap, ok := osInterface.(map[string]interface{})
				if !ok {
					return errors.New("os is not an object in version.json")
				}
				md5sum := md5.Sum(osBin)
				osMap["md5sum"] = hex.EncodeToString(md5sum[:])
				fmt.Printf("Replacing os.bin (md5 sum: %s)\n", osMap["md5sum"])
				versionFixed, err := json.MarshalIndent(version, "", "    ")
				if err != nil {
					return err
				}
				reader = bytes.NewReader(versionFixed)
				header.Size = int64(len(versionFixed))
			}
		}

		if err = tw.WriteHeader(header); err != nil {
			return err
		}

		if header.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, reader); err != nil {
				return err
			}
		}
	}
}
