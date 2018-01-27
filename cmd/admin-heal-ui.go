/*
 * Minio Client (C) 2018 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	humanize "github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/minio/mc/pkg/console"
	"github.com/minio/mc/pkg/probe"
	"github.com/minio/minio/pkg/madmin"
)

const (
	lineWidth = 80
)

// An alias of string to represent the health color code of an object
type hCol string

const (
	hColGrey   hCol = "Grey"
	hColRed         = "Red"
	hColYellow      = "Yellow"
	hColGreen       = "Green"
)

// getHPrintCol - map color code to color for printing
func getHPrintCol(c hCol) *color.Color {
	switch c {
	case hColGrey:
		return color.New(color.FgWhite, color.Bold)
	case hColRed:
		return color.New(color.FgRed, color.Bold)
	case hColYellow:
		return color.New(color.FgYellow, color.Bold)
	case hColGreen:
		return color.New(color.FgGreen, color.Bold)
	}
	return nil
}

var (
	hColOrder = []hCol{hColRed, hColYellow, hColGreen}
	hColTable = map[int][]int{
		1: {0, -1, 1},
		2: {0, 1, 2},
		3: {1, 2, 3},
		4: {1, 2, 4},
		5: {1, 3, 5},
		6: {2, 4, 6},
		7: {2, 4, 7},
		8: {2, 5, 8},
	}
)

func getHColCode(surplusShards, parityShards int) (c hCol, err error) {
	if parityShards < 1 || parityShards > 8 || surplusShards > parityShards {
		return c, fmt.Errorf("Invalid parity shard count/surplus shard count given")
	}
	if surplusShards < 0 {
		return hColGrey, err
	}
	colRow := hColTable[parityShards]
	for index, val := range colRow {
		if val != -1 && surplusShards <= val {
			return hColOrder[index], err
		}
	}
	return c, fmt.Errorf("cannot get a heal color code")
}

type uiData struct {
	Bucket, Prefix string
	Client         *madmin.AdminClient
	ClientToken    string
	ForceStart     bool
	HealOpts       *madmin.HealOpts
	LastItem       *hri

	// Total time since heal start
	HealDuration time.Duration

	// Accumulated statistics of heal result records
	BytesScanned int64

	// Counter for objects, and another counter for all kinds of
	// items
	ObjectsScanned, ItemsScanned int64

	// Counters for healed objects and all kinds of healed items
	ObjectsHealed, ItemsHealed int64

	// Map from online drives to number of objects with that many
	// online drives.
	ObjectsByOnlineDrives map[int]int64
	// Map of health color code to number of objects with that
	// health color code.
	HealthCols map[hCol]int64

	// channel to receive a prompt string to indicate activity on
	// the terminal
	CurChan (<-chan string)
}

func (ui *uiData) updateStats(i madmin.HealResultItem) error {
	if i.Type == madmin.HealItemObject {
		// Objects whose size could not be found have -1 size
		// returned.
		if i.ObjectSize >= 0 {
			ui.BytesScanned += i.ObjectSize
		}

		ui.ObjectsScanned++
	}
	ui.ItemsScanned++

	beforeUp, afterUp := i.GetOnlineCounts()
	if afterUp > beforeUp {
		if i.Type == madmin.HealItemObject {
			ui.ObjectsHealed++
		}
		ui.ItemsHealed++
	}
	ui.ObjectsByOnlineDrives[afterUp]++

	// Update health color stats:

	// Fetch health color after heal:
	var err error
	var afterCol hCol
	h := newHRI(&i)
	switch h.Type {
	case madmin.HealItemMetadata, madmin.HealItemBucket:
		_, afterCol, err = h.getReplicatedFileHCCChange()
	default:
		_, afterCol, err = h.getObjectHCCChange()
	}
	if err != nil {
		return err
	}

	ui.HealthCols[afterCol]++
	return nil
}

func (ui *uiData) updateDuration(s *madmin.HealTaskStatus) {
	ui.HealDuration = UTCNow().Sub(s.StartTime)
}

func (ui *uiData) getProgress() (oCount, objSize, duration string) {
	oCount = humanize.Comma(ui.ObjectsScanned)

	duration = ui.HealDuration.Round(time.Second).String()

	bytesScanned := float64(ui.BytesScanned)

	// Compute unit for object size
	magnitudes := []float64{1 << 10, 1 << 20, 1 << 30, 1 << 40, 1 << 50, 1 << 60}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	var i int
	for i = 0; i < len(magnitudes); i++ {
		if bytesScanned <= magnitudes[i] {
			break
		}
	}
	numUnits := int(bytesScanned * (1 << 10) / magnitudes[i])
	objSize = fmt.Sprintf("%d %s", numUnits, units[i])
	return
}

func (ui *uiData) getPercentsNBars() (p map[hCol]float64, b map[hCol]string) {
	// barChar, emptyBarChar := "█", "░"
	barChar, emptyBarChar := "█", " "
	barLen := 12
	sum := float64(ui.ItemsScanned)
	cols := []hCol{hColGrey, hColRed, hColYellow, hColGreen}

	p = make(map[hCol]float64, len(cols))
	b = make(map[hCol]string, len(cols))
	var filledLen int
	for _, col := range cols {
		v := float64(ui.HealthCols[col])
		if sum == 0 {
			p[col] = 0
			filledLen = 0
		} else {
			p[col] = v * 100 / sum
			// round up the filled part
			filledLen = int(math.Ceil(float64(barLen) * v / sum))
		}
		b[col] = strings.Repeat(barChar, filledLen) +
			strings.Repeat(emptyBarChar, barLen-filledLen)
	}
	return
}

func (ui *uiData) printItemsQuietly(s *madmin.HealTaskStatus) (err error) {
	lpad := func(s hCol) string {
		return fmt.Sprintf("%-6s", string(s))
	}
	rpad := func(s hCol) string {
		return fmt.Sprintf("%6s", string(s))
	}
	printColStr := func(before, after hCol) {
		console.PrintC("[" + lpad(before) + " -> " + rpad(after) + "] ")
	}

	var b, a hCol
	for _, item := range s.Items {
		h := newHRI(&item)
		switch h.Type {
		case madmin.HealItemMetadata, madmin.HealItemBucket:
			b, a, err = h.getReplicatedFileHCCChange()
		default:
			b, a, err = h.getObjectHCCChange()
		}
		if err != nil {
			return err
		}
		printColStr(b, a)
		hrStr := h.getHealResultStr()
		switch h.Type {
		case madmin.HealItemMetadata, madmin.HealItemBucketMetadata:
			console.PrintC(fmt.Sprintln("**", hrStr, "**"))
		default:
			console.PrintC(hrStr, "\n")
		}
	}
	return nil
}

func (ui *uiData) printStatsQuietly(s *madmin.HealTaskStatus) {
	totalObjects, totalSize, totalTime := ui.getProgress()

	healedStr := fmt.Sprintf("Healed:\t%s/%s objects; %s in %s\n",
		humanize.Comma(ui.ObjectsHealed), totalObjects,
		totalSize, totalTime)

	console.PrintC(healedStr)
}

func (ui *uiData) printItemsJSON(s *madmin.HealTaskStatus) (err error) {
	type change struct {
		Before string `json:"before"`
		After  string `json:"after"`
	}
	type healRec struct {
		Status string            `json:"status"`
		Error  string            `json:"error,omitempty"`
		Type   string            `json:"type"`
		Name   string            `json:"name"`
		Health change            `json:"health"`
		Drives map[string]change `json:"drives"`
		Size   int64             `json:"size"`
	}
	makeHR := func(h *hri) (r healRec, err error) {
		r.Status = "success"
		r.Type, r.Name = h.getHRTypeAndName()
		r.Drives = make(map[string]change)

		var b, a hCol
		switch h.Type {
		case madmin.HealItemMetadata, madmin.HealItemBucket:
			b, a, err = h.getReplicatedFileHCCChange()
		default:
			if h.Type == madmin.HealItemObject {
				r.Size = h.ObjectSize
			}
			b, a, err = h.getObjectHCCChange()
		}
		if err != nil {
			return r, err
		}
		r.Health.Before = strings.ToLower(string(b))
		r.Health.After = strings.ToLower(string(a))

		for k := range h.DriveInfo.Before {
			r.Drives[k] = change{h.DriveInfo.Before[k], h.DriveInfo.After[k]}
		}

		return r, nil
	}

	for _, item := range s.Items {
		h := newHRI(&item)
		r, err := makeHR(h)
		if err != nil {
			return err
		}
		jsonBytes, err := json.Marshal(r)
		fatalIf(probe.NewError(err), "Unable to marshal to JSON")
		console.Println(string(jsonBytes))
	}
	return nil
}

func (ui *uiData) printStatsJSON(s *madmin.HealTaskStatus) {
	var summary struct {
		Status         string `json:"status"`
		Error          string `json:"error,omitempty"`
		Type           string `json:"type"`
		ObjectsScanned int64  `json:"objects_scanned"`
		ObjectsHealed  int64  `json:"objects_healed"`
		ItemsScanned   int64  `json:"items_scanned"`
		ItemsHealed    int64  `json:"items_healed"`
		Size           int64  `json:"size"`
		ElapsedTime    int64  `json:"duration"`
	}

	summary.Status = "success"
	summary.Type = "summary"

	summary.ObjectsScanned = ui.ObjectsScanned
	summary.ObjectsHealed = ui.ObjectsHealed
	summary.ItemsScanned = ui.ItemsScanned
	summary.ItemsHealed = ui.ItemsHealed
	summary.Size = ui.BytesScanned
	summary.ElapsedTime = int64(ui.HealDuration.Round(time.Second).Seconds())

	jBytes, err := json.Marshal(summary)
	fatalIf(probe.NewError(err), "Unable to marshal to JSON")
	console.Println(string(jBytes))
}

func (ui *uiData) updateUI(s *madmin.HealTaskStatus) (err error) {
	itemCount := len(s.Items)
	h := ui.LastItem
	if itemCount > 0 {
		item := s.Items[itemCount-1]
		h = newHRI(&item)
		ui.LastItem = h
	}
	scannedStr := "** waiting for status from server **"
	if h != nil {
		scannedStr = lineTrunc(h.makeHealEntityString(), lineWidth-len("Scanned: "))
	}

	totalObjects, totalSize, totalTime := ui.getProgress()
	healedStr := fmt.Sprintf("%s/%s objects; %s in %s",
		humanize.Comma(ui.ObjectsHealed), totalObjects,
		totalSize, totalTime)

	console.Print(console.Colorize("HealUpdateUI", fmt.Sprintf(" %s", <-ui.CurChan)))
	console.PrintC(fmt.Sprintf("  %s\n", scannedStr))
	console.PrintC(fmt.Sprintf("    %s\n", healedStr))

	dspOrder := []hCol{hColGreen, hColYellow, hColRed, hColGrey}
	printColors := []*color.Color{}
	for _, c := range dspOrder {
		printColors = append(printColors, getHPrintCol(c))
	}
	t := console.NewTable(printColors, []bool{false, true, true}, 4)

	percentMap, barMap := ui.getPercentsNBars()
	cellText := make([][]string, len(dspOrder))
	for i := range cellText {
		cellText[i] = []string{
			string(dspOrder[i]),
			fmt.Sprintf(humanize.Comma(ui.HealthCols[dspOrder[i]])),
			fmt.Sprintf("%5.1f%% %s", percentMap[dspOrder[i]], barMap[dspOrder[i]]),
		}
	}

	t.DisplayTable(cellText)
	return nil
}

func (ui *uiData) UpdateDisplay(s *madmin.HealTaskStatus) (err error) {
	// Update state
	ui.updateDuration(s)
	for _, i := range s.Items {
		ui.updateStats(i)
	}

	// Update display
	switch {
	case globalJSON:
		err = ui.printItemsJSON(s)
	case globalQuiet:
		err = ui.printItemsQuietly(s)
	default:
		err = ui.updateUI(s)
	}
	return
}

func (ui *uiData) DisplayAndFollowHealStatus() (err error) {
	var res madmin.HealTaskStatus

	for {
		_, res, err = ui.Client.Heal(ui.Bucket, ui.Prefix, *ui.HealOpts,
			ui.ClientToken, ui.ForceStart)
		if err != nil {
			return err
		}

		err = ui.UpdateDisplay(&res)
		if err != nil {
			return err
		}

		if res.Summary == "finished" {
			break
		}

		if res.Summary == "stopped" {
			fmt.Println("Heal had an error - ", res.FailureDetail)
			break
		}

		time.Sleep(time.Second)
		if !globalQuiet && !globalJSON {
			console.RewindLines(8)
		}
	}
	if globalJSON {
		ui.printStatsJSON(&res)
		return nil
	}
	if globalQuiet {
		ui.printStatsQuietly(&res)
	}
	return nil
}
