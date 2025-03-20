package myaudio

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/gen2brain/malgo"
	"github.com/tphakala/birdnet-go/internal/birdnet"
	"github.com/tphakala/birdnet-go/internal/conf"
)

// captureSource holds information about an audio capture source.
type captureSource struct {
	Name    string
	ID      string
	Pointer unsafe.Pointer
}

// AudioDeviceInfo holds information about an audio device.
type AudioDeviceInfo struct {
	Index int
	Name  string
	ID    string
}

// AudioLevelData holds audio level data
type AudioLevelData struct {
	Level    int    `json:"level"`    // 0-100
	Clipping bool   `json:"clipping"` // true if clipping is detected
	Source   string `json:"source"`   // Source identifier (e.g., "malgo" for device, or RTSP URL)
	Name     string `json:"name"`     // Human-readable name of the source
}

// activeStreams keeps track of currently active RTSP streams
var activeStreams sync.Map

// ffmpegMonitor is the global FFmpeg process monitor
var ffmpegMonitor *FFmpegMonitor

// ListAudioSources returns a list of available audio capture devices.
func ListAudioSources() ([]AudioDeviceInfo, error) {
	// Create a slice to store audio device information
	var devices []AudioDeviceInfo

	// Initialize the audio context
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return devices, fmt.Errorf("failed to initialize context: %w", err)
	}

	// Ensure the context is uninitialized when the function returns
	defer func() {
		if err := ctx.Uninit(); err != nil {
			log.Printf("❌ failed to uninitialize context: %v", err)
		}
	}()

	// Get a list of capture devices
	infos, err := ctx.Devices(malgo.Capture)
	if err != nil {
		return devices, fmt.Errorf("failed to get devices: %w", err)
	}

	// Iterate through the list of devices
	for i := range infos {
		// Skip the discard/null device
		if strings.Contains(infos[i].Name(), "Discard all samples") {
			continue
		}

		// Decode the device ID from hexadecimal to ASCII
		decodedID, err := hexToASCII(infos[i].ID.String())
		if err != nil {
			log.Printf("❌ Error decoding ID for device %d: %v\n", i, err)
			continue
		}

		// Add the device information to the devices slice
		devices = append(devices, AudioDeviceInfo{
			Index: i,
			Name:  infos[i].Name(),
			ID:    decodedID,
		})
	}

	// Return the list of devices and nil error
	return devices, nil
}

// SetAudioDevice sets the audio device based on the provided device name.
func SetAudioDevice(deviceName string) (string, error) {
	// Initialize the audio context
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return "", fmt.Errorf("failed to initialize context: %w", err)
	}

	// Ensure the context is uninitialized when the function returns
	defer func() {
		if err := ctx.Uninit(); err != nil {
			log.Printf("❌ failed to uninitialize context: %v", err)
		}
	}()

	// Get a list of capture devices
	infos, err := ctx.Devices(malgo.Capture)
	if err != nil {
		return "", fmt.Errorf("failed to get devices: %w", err)
	}

	// Find the index of the device that matches the provided device name
	var index int
	for i := range infos {
		// Decode the device ID from hex to ASCII
		decodedID, err := hexToASCII(infos[i].ID.String())
		if err != nil {
			log.Printf("❌ Error decoding ID for device %d: %v\n", i, err)
			continue
		}

		// Check if the current device matches the specified settings
		if matchesDeviceSettings(decodedID, &infos[i], deviceName) {
			index = i
			break
		}
	}

	// Check if a valid device was found
	if index < 0 || index >= len(infos) {
		return "", fmt.Errorf("invalid device index")
	}

	// Configure the device
	deviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceConfig.Capture.Format = malgo.FormatS16    // 16-bit
	deviceConfig.Capture.Channels = conf.NumChannels // 1
	deviceConfig.Capture.DeviceID = infos[index].ID.Pointer()
	deviceConfig.SampleRate = conf.SampleRate // 48000
	deviceConfig.Alsa.NoMMap = 1

	// Initialize the device
	_, err = malgo.InitDevice(ctx.Context, deviceConfig, malgo.DeviceCallbacks{})
	if err != nil {
		return "", fmt.Errorf("failed to initialize device: %w", err)
	}

	// Return the name of the selected device
	return infos[index].Name(), nil
}

// ReconfigureRTSPStreams handles dynamic reconfiguration of RTSP streams
func ReconfigureRTSPStreams(settings *conf.Settings, wg *sync.WaitGroup, quitChan, restartChan chan struct{}, audioLevelChan chan AudioLevelData) {
	// If there are no RTSP URLs configured and FFmpeg monitor is running, stop it
	if len(settings.Realtime.RTSP.URLs) == 0 {
		if ffmpegMonitor != nil {
			ffmpegMonitor.Stop()
			ffmpegMonitor = nil
		}
		return
	}

	// Initialize FFmpeg monitor if not already running
	if ffmpegMonitor == nil {
		ffmpegMonitor = NewDefaultFFmpegMonitor()
		ffmpegMonitor.Start()
	}

	// Get current active streams
	currentStreams := make(map[string]bool)
	activeStreams.Range(func(key, value interface{}) bool {
		currentStreams[key.(string)] = true
		return true
	})

	// Stop streams that are no longer in settings
	for url := range currentStreams {
		found := false
		for _, newURL := range settings.Realtime.RTSP.URLs {
			if url == newURL {
				found = true
				break
			}
		}
		if !found {
			// Stream is no longer in settings, stop it
			if process, exists := ffmpegProcesses.Load(url); exists {
				if p, ok := process.(*FFmpegProcess); ok {
					// Stop the FFmpeg process first
					p.Cleanup(url)
					// Wait a short time for the process to fully stop
					time.Sleep(100 * time.Millisecond)
				}
			}

			// Mark stream as inactive before removing buffers
			activeStreams.Delete(url)
			log.Printf("⬇️ Stream %s removed", url)
			// Wait a short time for any in-flight writes to complete
			time.Sleep(100 * time.Millisecond)

			// Now it's safe to remove the buffers
			if err := RemoveAnalysisBuffer(url); err != nil {
				log.Printf("❌ Warning: failed to remove analysis buffer for %s: %v", url, err)
			}
			if err := RemoveCaptureBuffer(url); err != nil {
				log.Printf("❌ Warning: failed to remove capture buffer for %s: %v", url, err)
			}
		}
	}

	// Start new streams
	for _, url := range settings.Realtime.RTSP.URLs {
		// Check if stream is already active
		if _, exists := activeStreams.Load(url); exists {
			continue
		}

		abExists, cbExists := false, false //nolint:wastedassign // Need to initialize variables
		// Check if analysis buffer exists
		abMutex.RLock()
		_, abExists = analysisBuffers[url]
		abMutex.RUnlock()

		// Check if capture buffer exists
		cbMutex.RLock()
		_, cbExists = captureBuffers[url]
		cbMutex.RUnlock()

		// Initialize analysis buffer if it doesn't exist
		if !abExists {
			if err := AllocateAnalysisBuffer(conf.BufferSize*3, url); err != nil {
				log.Printf("❌ Failed to initialize analysis buffer for %s: %v", url, err)
				continue
			}
		}

		// Initialize capture buffer if it doesn't exist
		if !cbExists {
			if err := AllocateCaptureBuffer(60, conf.SampleRate, conf.BitDepth/8, url); err != nil {
				// Clean up the ring buffer if audio buffer init fails and we just created it
				if !abExists {
					err := RemoveCaptureBuffer(url)
					if err != nil {
						log.Printf("❌ Failed to remove capture buffer for %s: %v", url, err)
					}
				}
				log.Printf("❌ Failed to initialize capture buffer for %s: %v", url, err)
				continue
			}
		}

		// New stream, start it
		activeStreams.Store(url, true)
		go CaptureAudioRTSP(url, settings.Realtime.RTSP.Transport, wg, quitChan, restartChan, audioLevelChan)
	}
}

// CaptureAudio captures audio from the specified device.
func CaptureAudio(settings *conf.Settings, wg *sync.WaitGroup, quitChan, restartChan chan struct{}, audioLevelChan chan AudioLevelData) {
	// If no RTSP URLs and no audio device configured, return early
	if len(settings.Realtime.RTSP.URLs) == 0 && settings.Realtime.Audio.Source == "" {
		return
	}

	// Add to the wait group
	wg.Add(1)

	// Create a new audio capture service
	service, err := NewAudioCaptureService(settings)
	if err != nil {
		log.Printf("❌ Failed to create audio capture service: %v", err)
		wg.Done() // Signal we're done if we failed
		return
	}

	// Connect external channels to the service
	service.ConnectExternalChannels(restartChan)

	// Set the BirdNET instance for analysis
	if bn := birdnet.GetGlobalInstance(); bn != nil {
		service.SetBirdNET(bn)

		// Also register the default instance
		service.RegisterModelInstance("default", bn)
	} else {
		log.Printf("⚠️ BirdNET instance not available, analysis buffer monitors will not be started")
	}

	// Forward audio level data to the caller's channel
	go func() {
		for levelData := range service.GetAudioLevelChannel() {
			audioLevelChan <- levelData
		}
	}()

	// Start the service
	if err := service.Start(); err != nil {
		log.Printf("❌ Failed to start audio capture service: %v", err)
		wg.Done() // Signal we're done if we failed
		return
	}

	// Wait for quit signal in a separate goroutine
	go func() {
		<-quitChan
		// Stop the service
		service.Stop()
		// Signal that capture has completed
		wg.Done()
	}()
}

// isHardwareDevice checks if the device ID indicates a hardware device
func isHardwareDevice(decodedID string) bool {
	// On Linux, hardware devices have IDs in the format ":X,Y"
	if runtime.GOOS == "linux" {
		return strings.Contains(decodedID, ":") && strings.Contains(decodedID, ",")
	}
	// On Windows and macOS, consider all devices as potential hardware devices
	// as the ID format is different and we rely on the OS's device enumeration
	return true
}

// getHardwareDevices filters the device infos to return only hardware devices
func getHardwareDevices(infos []malgo.DeviceInfo) []malgo.DeviceInfo {
	var hardwareDevices []malgo.DeviceInfo
	for i := range infos {
		decodedID, err := hexToASCII(infos[i].ID.String())
		if err != nil {
			continue
		}
		if isHardwareDevice(decodedID) {
			hardwareDevices = append(hardwareDevices, infos[i])
		}
	}
	return hardwareDevices
}

// TestCaptureDevice tests if a capture device can be initialized and started.
// Returns true if the device is working, false otherwise.
func TestCaptureDevice(ctx *malgo.AllocatedContext, info *malgo.DeviceInfo) bool {
	deviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceConfig.Capture.Format = malgo.FormatS16
	deviceConfig.Capture.Channels = conf.NumChannels
	deviceConfig.Capture.DeviceID = info.ID.Pointer()
	deviceConfig.SampleRate = conf.SampleRate
	deviceConfig.Alsa.NoMMap = 1

	// Try to initialize the device
	device, err := malgo.InitDevice(ctx.Context, deviceConfig, malgo.DeviceCallbacks{})
	if err != nil {
		return false
	}
	defer device.Uninit()

	// Try to start the device
	if err := device.Start(); err != nil {
		return false
	}

	// Stop the device
	_ = device.Stop()
	return true
}

// ValidateAudioDevice checks if the configured audio source is available and working.
// Returns an error if the device is not available or not working.
// This function also updates the settings if the device is not valid.
func ValidateAudioDevice(settings *conf.Settings) error {
	if settings.Realtime.Audio.Source == "" {
		return nil
	}

	var backend malgo.Backend
	switch runtime.GOOS {
	case "linux":
		backend = malgo.BackendAlsa
	case "windows":
		backend = malgo.BackendWasapi
	case "darwin":
		backend = malgo.BackendCoreaudio
	}

	// Initialize malgo context
	malgoCtx, err := malgo.InitContext([]malgo.Backend{backend}, malgo.ContextConfig{}, nil)
	if err != nil {
		settings.Realtime.Audio.Source = ""
		return fmt.Errorf("failed to initialize audio context: %w", err)
	}
	defer malgoCtx.Uninit() //nolint:errcheck // We handle errors in the caller

	// Get list of capture devices
	infos, err := malgoCtx.Devices(malgo.Capture)
	if err != nil {
		settings.Realtime.Audio.Source = ""
		return fmt.Errorf("failed to get capture devices: %w", err)
	}

	// Filter to get only hardware devices to check if any are available
	hardwareDevices := getHardwareDevices(infos)
	if len(hardwareDevices) == 0 {
		settings.Realtime.Audio.Source = ""
		return fmt.Errorf("no hardware audio capture devices found")
	}

	// Try to find and test the configured device, in this we also accept alsa speudo devices
	for i := range infos {
		decodedID, err := hexToASCII(infos[i].ID.String())
		if err != nil {
			continue
		}

		if matchesDeviceSettings(decodedID, &infos[i], settings.Realtime.Audio.Source) {
			if TestCaptureDevice(malgoCtx, &infos[i]) {
				return nil
			}
			settings.Realtime.Audio.Source = ""
			return fmt.Errorf("configured audio device '%s' failed hardware test", settings.Realtime.Audio.Source)
		}
	}

	//settings.Realtime.Audio.Source = ""
	return fmt.Errorf("configured audio device '%s' not found", settings.Realtime.Audio.Source)
}

// selectCaptureSource selects and tests an appropriate capture device based on the provided settings.
func selectCaptureSource(settings *conf.Settings) (captureSource, error) {
	var backend malgo.Backend
	switch runtime.GOOS {
	case "linux":
		backend = malgo.BackendAlsa
	case "windows":
		backend = malgo.BackendWasapi
	case "darwin":
		backend = malgo.BackendCoreaudio
	}

	malgoCtx, err := malgo.InitContext([]malgo.Backend{backend}, malgo.ContextConfig{}, func(message string) {
		if settings.Debug {
			fmt.Print(message)
		}
	})
	if err != nil {
		return captureSource{}, fmt.Errorf("audio context initialization failed: %w", err)
	}
	defer malgoCtx.Uninit() //nolint:errcheck // We handle errors in the caller

	// Get list of capture sources
	infos, err := malgoCtx.Devices(malgo.Capture)
	if err != nil {
		return captureSource{}, fmt.Errorf("failed to get capture devices: %w", err)
	}

	fmt.Println("Available Capture Sources:")
	for i := range infos {
		decodedID, err := hexToASCII(infos[i].ID.String())
		if err != nil {
			fmt.Printf("❌ Error decoding ID for device %d: %v\n", i, err)
			continue
		}

		output := fmt.Sprintf("  %d: %s", i, infos[i].Name())
		if runtime.GOOS == "linux" {
			output = fmt.Sprintf("%s, %s", output, decodedID)
		}

		if matchesDeviceSettings(decodedID, &infos[i], settings.Realtime.Audio.Source) {
			if TestCaptureDevice(malgoCtx, &infos[i]) {
				fmt.Printf("%s (✅ selected)\n", output)
				return captureSource{
					Name:    infos[i].Name(),
					ID:      decodedID,
					Pointer: infos[i].ID.Pointer(),
				}, nil
			}
			fmt.Printf("%s (❌ device test failed)\n", output)
			continue
		}
		fmt.Println(output)
	}

	return captureSource{}, fmt.Errorf("no working capture device found matching '%s'", settings.Realtime.Audio.Source)
}

// matchesDeviceSettings checks if the device matches the settings specified by the user.
func matchesDeviceSettings(decodedID string, info *malgo.DeviceInfo, audioSource string) bool {
	if runtime.GOOS == "windows" && audioSource == "sysdefault" {
		// On Windows, there is no "sysdefault" device. Use miniaudio's default device instead.
		return info.IsDefault == 1
	}
	// Check if the decoded ID or device name matches the user's setting.
	return decodedID == audioSource || strings.Contains(info.Name(), audioSource)
}

// hexToASCII converts a hexadecimal string to an ASCII string.
func hexToASCII(hexStr string) (string, error) {
	bytes, err := hex.DecodeString(hexStr)
	if err != nil {
		return "", err // return the error if hex decoding fails
	}
	return string(bytes), nil
}

// calculateAudioLevel calculates the RMS (Root Mean Square) of the audio samples
// and returns an AudioLevelData struct with the level and clipping status
func calculateAudioLevel(samples []byte, source, name string) AudioLevelData {
	// If there are no samples, return zero level and no clipping
	if len(samples) == 0 {
		return AudioLevelData{Level: 0, Clipping: false, Source: source, Name: name}
	}

	// Ensure we have an even number of bytes (16-bit samples)
	if len(samples)%2 != 0 {
		// Truncate to even number of bytes
		samples = samples[:len(samples)-1]
	}

	var sum float64
	sampleCount := len(samples) / 2 // 2 bytes per sample for 16-bit audio
	isClipping := false
	maxSample := float64(0)

	// Iterate through samples, calculating sum of squares and checking for clipping
	for i := 0; i < len(samples); i += 2 {
		if i+1 >= len(samples) {
			break
		}

		// Convert two bytes to a 16-bit sample
		sample := int16(binary.LittleEndian.Uint16(samples[i : i+2]))
		sampleAbs := math.Abs(float64(sample))
		sum += sampleAbs * sampleAbs

		// Keep track of the maximum sample value
		if sampleAbs > maxSample {
			maxSample = sampleAbs
		}

		// Check for clipping (maximum positive or negative 16-bit value)
		if sample == 32767 || sample == -32768 {
			isClipping = true
		}
	}

	// If we ended up with no samples, return zero level and no clipping
	if sampleCount == 0 {
		return AudioLevelData{Level: 0, Clipping: false, Source: source, Name: name}
	}

	// Calculate Root Mean Square (RMS)
	rms := math.Sqrt(sum / float64(sampleCount))

	// Convert RMS to decibels
	// 32768 is max value for 16-bit audio
	db := 20 * math.Log10(rms/32768.0)

	// Scale decibels to 0-100 range
	// Adjust the range to make it more sensitive
	scaledLevel := (db + 60) * (100.0 / 50.0)

	// If the audio is clipping, ensure the level is at or near 100
	if isClipping {
		scaledLevel = math.Max(scaledLevel, 95)
	}

	// Clamp the value between 0 and 100
	if scaledLevel < 0 {
		scaledLevel = 0
	} else if scaledLevel > 100 {
		scaledLevel = 100
	}

	// Return the calculated audio level data
	return AudioLevelData{
		Level:    int(scaledLevel),
		Clipping: isClipping,
		Source:   source,
		Name:     name,
	}
}
