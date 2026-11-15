package escl

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// XML namespaces used by eSCL. Decoding matches on local names only, because
// implementations differ in prefixes and occasionally in namespaces; encoding
// uses these URIs.
const (
	nsScan = "http://schemas.hp.com/imaging/escl/2011/05/03"
	nsPWG  = "http://www.pwg.org/schemas/2010/12/sm"
)

// Scanner states (pwg:State).
const (
	StateIdle       = "Idle"
	StateProcessing = "Processing"
	StateTesting    = "Testing"
	StateStopped    = "Stopped"
	StateDown       = "Down"
)

// ADF states (scan:AdfState) seen in the wild. Only AdfLoaded means "paper is
// ready"; every other value, including unknown ones, counts as not loaded.
const (
	AdfEmpty               = "ScannerAdfEmpty"
	AdfLoaded              = "ScannerAdfLoaded"
	AdfProcessing          = "ScannerAdfProcessing"
	AdfJam                 = "ScannerAdfJam"
	AdfMispick             = "ScannerAdfMispick"
	AdfHatchOpen           = "ScannerAdfHatchOpen"
	AdfDoorOpen            = "ScannerAdfDoorOpen"
	AdfMultipickDetected   = "ScannerAdfMultipickDetected"
	AdfInputTrayFailed     = "ScannerAdfInputTrayFailed"
	AdfInputTrayOverloaded = "ScannerAdfInputTrayOverloaded"
	AdfDuplexPageTooShort  = "ScannerAdfDuplexPageTooShort"
	AdfDuplexPageTooLong   = "ScannerAdfDuplexPageTooLong"
	AdfHopperEmpty         = "ScannerAdfHopperEmpty"
)

// KnownAdfStates lists the ADF states above, for metrics pre-registration.
var KnownAdfStates = []string{
	AdfEmpty, AdfLoaded, AdfProcessing, AdfJam, AdfMispick, AdfHatchOpen,
	AdfDoorOpen, AdfMultipickDetected, AdfInputTrayFailed, AdfInputTrayOverloaded,
	AdfDuplexPageTooShort, AdfDuplexPageTooLong, AdfHopperEmpty,
}

// AdfIsEmpty reports whether the feeder is known to be empty.
func AdfIsEmpty(state string) bool {
	return state == AdfEmpty || state == AdfHopperEmpty
}

// AdfIsFault reports whether the ADF state describes a paper handling fault.
// A feeder job that ends while the ADF reports a fault must not be delivered.
func AdfIsFault(state string) bool {
	switch state {
	case AdfJam, AdfMispick, AdfHatchOpen, AdfDoorOpen, AdfMultipickDetected,
		AdfInputTrayFailed, AdfInputTrayOverloaded, AdfDuplexPageTooShort, AdfDuplexPageTooLong:
		return true
	}
	return false
}

// Job states (pwg:JobState).
const (
	JobPending    = "Pending"
	JobProcessing = "Processing"
	JobCompleted  = "Completed"
	JobCanceled   = "Canceled"
	JobAborted    = "Aborted"
)

// Status is the parsed /eSCL/ScannerStatus document.
type Status struct {
	Version  string    `xml:"Version"`
	State    string    `xml:"State"`
	AdfState string    `xml:"AdfState"`
	Jobs     []JobInfo `xml:"Jobs>JobInfo"`
}

// AdfLoaded reports whether paper is in the feeder.
func (s *Status) AdfLoaded() bool { return s.AdfState == AdfLoaded }

// Job returns the status entry of the job with the given URL, UUID or path.
// The device lists JobUri as a path, while the Location of a new job is an
// absolute URL, so entries are matched by UUID or by trailing path.
func (s *Status) Job(job string) (JobInfo, bool) {
	uuid := job[strings.LastIndex(job, "/")+1:]
	for _, j := range s.Jobs {
		if (j.JobUUID != "" && j.JobUUID == uuid) ||
			(j.JobURI != "" && strings.HasSuffix(job, j.JobURI)) {
			return j, true
		}
	}
	return JobInfo{}, false
}

// JobInfo is one entry of the job list in ScannerStatus.
type JobInfo struct {
	JobURI           string   `xml:"JobUri"`
	JobUUID          string   `xml:"JobUuid"`
	Age              int      `xml:"Age"`
	ImagesCompleted  int      `xml:"ImagesCompleted"`
	ImagesToTransfer int      `xml:"ImagesToTransfer"`
	JobState         string   `xml:"JobState"`
	JobStateReasons  []string `xml:"JobStateReasons>JobStateReason"`
}

// Capabilities is the subset of /eSCL/ScannerCapabilities this service uses.
type Capabilities struct {
	Version      string     `xml:"Version"`
	MakeAndModel string     `xml:"MakeAndModel"`
	Platen       *InputCaps `xml:"Platen>PlatenInputCaps"`
	Adf          *AdfCaps   `xml:"Adf"`
}

// AdfCaps describes the automatic document feeder.
type AdfCaps struct {
	Simplex        *InputCaps `xml:"AdfSimplexInputCaps"`
	Duplex         *InputCaps `xml:"AdfDuplexInputCaps"`
	FeederCapacity int        `xml:"FeederCapacity"`
	XImagePosition string     `xml:"Justification>XImagePosition"`
	YImagePosition string     `xml:"Justification>YImagePosition"`
	Options        []string   `xml:"AdfOptions>AdfOption"`
}

// InputCaps describes one input source. Dimensions are in 1/300 inch.
type InputCaps struct {
	MinWidth    int      `xml:"MinWidth"`
	MaxWidth    int      `xml:"MaxWidth"`
	MinHeight   int      `xml:"MinHeight"`
	MaxHeight   int      `xml:"MaxHeight"`
	ColorModes  []string `xml:"SettingProfiles>SettingProfile>ColorModes>ColorMode"`
	Formats     []string `xml:"SettingProfiles>SettingProfile>DocumentFormats>DocumentFormat"`
	Resolutions []int    `xml:"SettingProfiles>SettingProfile>SupportedResolutions>DiscreteResolutions>DiscreteResolution>XResolution"`
	Intents     []string `xml:"SupportedIntents>Intent"`
}

// InputSource selects the platen (flatbed) or the feeder.
type InputSource string

// Input sources.
const (
	Platen InputSource = "Platen"
	Feeder InputSource = "Feeder"
)

// Region is a scan region in 1/300 inch.
type Region struct {
	XOffset, YOffset, Width, Height int
}

// ScanSettings is the body of a scan job request.
type ScanSettings struct {
	Source     InputSource
	Intent     string // Document, TextAndGraphic, Photo, Preview
	ColorMode  string // BlackAndWhite1, Grayscale8, RGB24
	Resolution int    // dpi, used for X and Y
	Region     Region
	Format     string // MIME type; application/pdf when empty
}

// marshal renders the settings in the form the WF-2960 accepts (see
// escltest/testdata/scan-settings-feeder.xml). scan:Duplex is only sent for
// the feeder.
func (s ScanSettings) marshal() []byte {
	format := s.Format
	if format == "" {
		format = "application/pdf"
	}
	var b strings.Builder
	b.WriteString(xml.Header)
	fmt.Fprintf(&b, `<scan:ScanSettings xmlns:scan=%q xmlns:pwg=%q>`, nsScan, nsPWG)
	b.WriteString(`<pwg:Version>2.0</pwg:Version>`)
	writeElem(&b, "scan:Intent", s.Intent)
	b.WriteString(`<pwg:ScanRegions><pwg:ScanRegion>`)
	b.WriteString(`<pwg:ContentRegionUnits>escl:ThreeHundredthsOfInches</pwg:ContentRegionUnits>`)
	fmt.Fprintf(&b, `<pwg:XOffset>%d</pwg:XOffset><pwg:YOffset>%d</pwg:YOffset>`, s.Region.XOffset, s.Region.YOffset)
	fmt.Fprintf(&b, `<pwg:Width>%d</pwg:Width><pwg:Height>%d</pwg:Height>`, s.Region.Width, s.Region.Height)
	b.WriteString(`</pwg:ScanRegion></pwg:ScanRegions>`)
	writeElem(&b, "pwg:InputSource", string(s.Source))
	writeElem(&b, "scan:ColorMode", s.ColorMode)
	writeElem(&b, "pwg:DocumentFormat", format)
	writeElem(&b, "scan:DocumentFormatExt", format)
	fmt.Fprintf(&b, `<scan:XResolution>%d</scan:XResolution><scan:YResolution>%d</scan:YResolution>`, s.Resolution, s.Resolution)
	if s.Source == Feeder {
		b.WriteString(`<scan:Duplex>false</scan:Duplex>`)
	}
	b.WriteString(`</scan:ScanSettings>`)
	return []byte(b.String())
}

func writeElem(b *strings.Builder, name, value string) {
	b.WriteString("<" + name + ">")
	_ = xml.EscapeText(b, []byte(value)) // strings.Builder never fails
	b.WriteString("</" + name + ">")
}
