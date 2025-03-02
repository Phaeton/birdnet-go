// birdnet.go BirdNET model specific code
package birdnet

import (
	"archive/zip"
	"bufio"
	"bytes"
	_ "embed" // Embedding data directly into the binary.
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/tphakala/birdnet-go/internal/conf"
	"github.com/tphakala/birdnet-go/internal/cpuspec"
	"github.com/tphakala/birdnet-go/internal/logger"
	tflite "github.com/tphakala/go-tflite"
	"github.com/tphakala/go-tflite/delegates/xnnpack"
)

// Embedded TensorFlow Lite model data.
//
//go:embed data/BirdNET_GLOBAL_6K_V2.4_Model_FP32.tflite
var modelData []byte

// Embedded TensorFlow Lite range filter model data.
//
//go:embed data/BirdNET_GLOBAL_6K_V2.4_MData_Model_FP16.tflite
var metaModelDataV1 []byte

//go:embed data/BirdNET_GLOBAL_6K_V2.4_MData_Model_V2_FP16.tflite
var metaModelDataV2 []byte

// Model version string, default is the embedded model version
var modelVersion = "BirdNET GLOBAL 6K V2.4 FP32"

// Embedded labels in zip format.
//
//go:embed data/labels.zip
var labelsZip []byte

// BirdNET struct represents the BirdNET model with interpreters and configuration.
type BirdNET struct {
	AnalysisInterpreter *tflite.Interpreter
	RangeInterpreter    *tflite.Interpreter
	Settings            *conf.Settings
	logger              *logger.Logger
	mu                  sync.Mutex
}

// NewBirdNET initializes a new BirdNET instance with given settings.
func NewBirdNET(settings *conf.Settings, parentLogger *logger.Logger) (*BirdNET, error) {
	var bnLogger *logger.Logger
	if parentLogger != nil {
		bnLogger = parentLogger.Named("birdnet")
	} else {
		// Fall back to global logger if no parent logger is provided
		bnLogger = logger.Named("birdnet")
	}

	bn := &BirdNET{
		Settings: settings,
		logger:   bnLogger,
	}

	if err := bn.initializeModel(); err != nil {
		return nil, fmt.Errorf("failed to initialize model: %w", err)
	}

	if err := bn.initializeMetaModel(); err != nil {
		return nil, fmt.Errorf("failed to initialize meta model: %w", err)
	}

	if err := bn.loadLabels(); err != nil {
		return nil, fmt.Errorf("failed to load labels: %w", err)
	}

	// Normalize and validate locale setting.
	inputLocale := strings.ToLower(settings.BirdNET.Locale)
	normalizedLocale, err := conf.NormalizeLocale(inputLocale)
	if err != nil {
		return nil, err
	}
	settings.BirdNET.Locale = normalizedLocale

	return bn, nil
}

// initializeModel loads and initializes the primary BirdNET model.
func (bn *BirdNET) initializeModel() error {
	modelData, err := bn.loadModel()
	if err != nil {
		return err
	}

	model := tflite.NewModel(modelData)
	if model == nil {
		return fmt.Errorf("cannot load model")
	}

	// Determine the number of threads for the interpreter based on settings and system capacity.
	threads := bn.determineThreadCount(bn.Settings.BirdNET.Threads)

	// Configure interpreter options.
	options := tflite.NewInterpreterOptions()

	// Try to use XNNPACK delegate if enabled in settings
	if bn.Settings.BirdNET.UseXNNPACK {
		delegate := xnnpack.New(xnnpack.DelegateOptions{NumThreads: int32(max(1, threads-1))})
		if delegate == nil {
			bn.logger.Warn("Failed to create XNNPACK delegate, falling back to default CPU",
				"recommendation", "Download updated tensorflow lite C API library from: https://github.com/tphakala/tflite_c/releases/tag/v2.17.1")
			options.SetNumThread(threads)
		} else {
			options.AddDelegate(delegate)
			options.SetNumThread(1)
		}
	} else {
		options.SetNumThread(threads)
	}

	options.SetErrorReporter(func(msg string, user_data interface{}) {
		bn.logger.Error(msg)
	}, nil)

	// Create and allocate the TensorFlow Lite interpreter.
	bn.AnalysisInterpreter = tflite.NewInterpreter(model, options)
	if bn.AnalysisInterpreter == nil {
		return fmt.Errorf("cannot create interpreter")
	}
	if status := bn.AnalysisInterpreter.AllocateTensors(); status != tflite.OK {
		return fmt.Errorf("tensor allocation failed")
	}

	// Replace model version if custom model is used
	if bn.Settings.BirdNET.ModelPath != "" {
		modelVersion = bn.Settings.BirdNET.ModelPath
	}

	// Get CPU information for detailed message
	var initMessage string
	if bn.Settings.BirdNET.Threads == 0 {
		spec := cpuspec.GetCPUSpec()
		if spec.PerformanceCores > 0 {
			initMessage = fmt.Sprintf("%s model initialized, optimized to use %v threads on %v P-cores (system has %v total CPUs)",
				modelVersion, threads, spec.PerformanceCores, runtime.NumCPU())
		} else {
			initMessage = fmt.Sprintf("%s model initialized, using %v threads of available %v CPUs",
				modelVersion, threads, runtime.NumCPU())
		}
	} else {
		initMessage = fmt.Sprintf("%s model initialized, using configured %v threads of available %v CPUs",
			modelVersion, threads, runtime.NumCPU())
	}
	bn.logger.Info(initMessage)
	return nil
}

// getMetaModelData returns the appropriate meta model data based on the settings.
func (bn *BirdNET) getMetaModelData() []byte {
	if bn.Settings.BirdNET.RangeFilter.Model == "legacy" {
		bn.logger.Warn("Using legacy range filter model")
		return metaModelDataV1
	}
	return metaModelDataV2
}

// initializeMetaModel loads and initializes the meta model used for range filtering.
func (bn *BirdNET) initializeMetaModel() error {
	metaModelData := bn.getMetaModelData()

	model := tflite.NewModel(metaModelData)
	if model == nil {
		return fmt.Errorf("cannot load meta model from embedded data")
	}

	// Meta model requires only one CPU.
	options := tflite.NewInterpreterOptions()
	options.SetNumThread(1)
	options.SetErrorReporter(func(msg string, user_data interface{}) {
		bn.logger.Error(msg)
	}, nil)

	// Create and allocate the TensorFlow Lite interpreter for the meta model.
	bn.RangeInterpreter = tflite.NewInterpreter(model, options)
	if bn.RangeInterpreter == nil {
		return fmt.Errorf("cannot create meta model interpreter")
	}
	if status := bn.RangeInterpreter.AllocateTensors(); status != tflite.OK {
		return fmt.Errorf("tensor allocation failed for meta model")
	}

	return nil
}

// determineThreadCount calculates the appropriate number of threads to use based on settings and system capabilities.
func (bn *BirdNET) determineThreadCount(configuredThreads int) int {
	systemCpuCount := runtime.NumCPU()

	// If threads are configured to 0, try to get optimal count from cpuspec
	if configuredThreads == 0 {
		spec := cpuspec.GetCPUSpec()
		optimalThreads := spec.GetOptimalThreadCount()
		if optimalThreads > 0 {
			return min(optimalThreads, systemCpuCount)
		}

		// If cpuspec doesn't know the CPU, use all available cores
		return systemCpuCount
	}

	// If threads are configured but exceed system CPU count, limit to system CPU count
	if configuredThreads > systemCpuCount {
		return systemCpuCount
	}

	return configuredThreads
}

// loadLabels extracts and loads labels from either the embedded zip file or an external file
func (bn *BirdNET) loadLabels() error {
	bn.Settings.BirdNET.Labels = []string{} // Reset labels.

	// Use embedded labels if no external label path is set
	if bn.Settings.BirdNET.LabelPath == "" {
		return bn.loadEmbeddedLabels()
	}

	// Otherwise use external labels
	return bn.loadExternalLabels()
}

func (bn *BirdNET) loadEmbeddedLabels() error {
	reader := bytes.NewReader(labelsZip)
	zipReader, err := zip.NewReader(reader, int64(len(labelsZip)))
	if err != nil {
		return err
	}

	// if locale is not set use english as default
	if bn.Settings.BirdNET.Locale == "" {
		bn.logger.Info("BirdNET locale not set, using English as default")
		bn.Settings.BirdNET.Locale = "en"
	}

	labelFileName := fmt.Sprintf("labels_%s.txt", bn.Settings.BirdNET.Locale)
	for _, file := range zipReader.File {
		if file.Name == labelFileName {
			return bn.readLabelFile(file)
		}
	}
	return fmt.Errorf("label file '%s' not found in the zip archive", labelFileName)
}

func (bn *BirdNET) loadExternalLabels() error {
	file, err := os.Open(bn.Settings.BirdNET.LabelPath)
	if err != nil {
		return fmt.Errorf("failed to open external label file: %w", err)
	}
	defer file.Close()

	// Read the first 4 bytes to check if it's a zip file
	header := make([]byte, 4)
	if _, err := file.Read(header); err != nil {
		return fmt.Errorf("failed to read file header: %w", err)
	}

	// Reset the file pointer to the beginning
	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to reset file pointer: %w", err)
	}

	// Check if it's a zip file (ZIP files start with "PK\x03\x04")
	if bytes.Equal(header, []byte("PK\x03\x04")) {
		return bn.loadLabelsFromZip(file)
	}

	// If not a zip file, treat it as a plain text file
	return bn.loadLabelsFromText(file)
}

func (bn *BirdNET) loadLabelsFromZip(file *os.File) error {
	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to get file info: %w", err)
	}
	zipReader, err := zip.NewReader(file, fileInfo.Size())
	if err != nil {
		return fmt.Errorf("failed to create zip reader: %w", err)
	}

	labelFileName := fmt.Sprintf("labels_%s.txt", bn.Settings.BirdNET.Locale)
	for _, zipFile := range zipReader.File {
		if zipFile.Name == labelFileName {
			return bn.readLabelFile(zipFile)
		}
	}
	return fmt.Errorf("label file '%s' not found in the zip archive", labelFileName)
}

func (bn *BirdNET) loadLabelsFromText(file *os.File) error {
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		bn.Settings.BirdNET.Labels = append(bn.Settings.BirdNET.Labels, strings.TrimSpace(scanner.Text()))
	}
	return scanner.Err()
}

// readLabelFile reads and processes the label file from the zip archive.
func (bn *BirdNET) readLabelFile(file *zip.File) error {
	fileReader, err := file.Open()
	if err != nil {
		return err
	}
	defer fileReader.Close()

	scanner := bufio.NewScanner(fileReader)
	for scanner.Scan() {
		bn.Settings.BirdNET.Labels = append(bn.Settings.BirdNET.Labels, strings.TrimSpace(scanner.Text()))
	}
	return scanner.Err() // Returns nil if no errors occurred during scanning.
}

// Delete releases resources used by the TensorFlow Lite interpreters.
func (bn *BirdNET) Delete() {
	if bn.AnalysisInterpreter != nil {
		bn.AnalysisInterpreter.Delete()
	}
	if bn.RangeInterpreter != nil {
		bn.RangeInterpreter.Delete()
	}
}

// loadModel loads either the embedded model or an external model file
func (bn *BirdNET) loadModel() ([]byte, error) {
	if bn.Settings.BirdNET.ModelPath == "" {
		return modelData, nil
	}

	modelPath := bn.Settings.BirdNET.ModelPath
	data, err := os.ReadFile(modelPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read model file: %w", err)
	}
	return data, nil
}

// validateModelAndLabels checks if the number of labels matches the model's output size
func (bn *BirdNET) validateModelAndLabels() error {
	// Get the output tensor to check its dimensions
	outputTensor := bn.AnalysisInterpreter.GetOutputTensor(0)
	if outputTensor == nil {
		return fmt.Errorf("cannot get output tensor")
	}

	// Get the number of classes from the model's output tensor
	modelOutputSize := outputTensor.Dim(outputTensor.NumDims() - 1)

	// Compare with the number of labels
	if len(bn.Settings.BirdNET.Labels) != modelOutputSize {
		bn.logger.Error("Label count mismatch",
			"expected", modelOutputSize,
			"actual", len(bn.Settings.BirdNET.Labels),
			"status", "❌")
		return fmt.Errorf("label count mismatch: model expects %d classes but label file has %d labels",
			modelOutputSize, len(bn.Settings.BirdNET.Labels))
	}

	bn.logger.Info("Model validation successful",
		"labels", modelOutputSize,
		"status", "✅")
	return nil
}

// ReloadModel safely reloads the BirdNET model and labels while handling ongoing analysis
func (bn *BirdNET) ReloadModel() error {
	bn.Debug("Acquiring mutex for model reload", "status", "🔒")
	bn.mu.Lock()
	defer bn.mu.Unlock()
	bn.Debug("Acquired mutex for model reload", "status", "✅")

	// Store old interpreters to clean up after successful reload
	oldAnalysisInterpreter := bn.AnalysisInterpreter
	oldRangeInterpreter := bn.RangeInterpreter

	// Initialize new model
	if err := bn.initializeModel(); err != nil {
		bn.logger.Error("Failed to reload model", "error", err, "status", "❌")
		return fmt.Errorf("failed to reload model: %w", err)
	}
	bn.Debug("Model initialized successfully", "status", "✅")

	// Initialize new meta model
	if err := bn.initializeMetaModel(); err != nil {
		// Clean up the newly created analysis interpreter if meta model fails
		if bn.AnalysisInterpreter != nil {
			bn.AnalysisInterpreter.Delete()
		}
		// Restore the old interpreters
		bn.AnalysisInterpreter = oldAnalysisInterpreter
		bn.RangeInterpreter = oldRangeInterpreter
		bn.logger.Error("Failed to reload meta model", "error", err, "status", "❌")
		return fmt.Errorf("failed to reload meta model: %w", err)
	}
	bn.Debug("Meta model initialized successfully", "status", "✅")

	// Reload labels
	if err := bn.loadLabels(); err != nil {
		// Clean up the newly created interpreters if label loading fails
		if bn.AnalysisInterpreter != nil {
			bn.AnalysisInterpreter.Delete()
		}
		if bn.RangeInterpreter != nil {
			bn.RangeInterpreter.Delete()
		}
		// Restore the old interpreters
		bn.AnalysisInterpreter = oldAnalysisInterpreter
		bn.RangeInterpreter = oldRangeInterpreter
		bn.logger.Error("Failed to reload labels", "error", err, "status", "❌")
		return fmt.Errorf("failed to reload labels: %w", err)
	}
	bn.Debug("Labels loaded successfully", "status", "✅")

	// Validate that the model and labels match
	if err := bn.validateModelAndLabels(); err != nil {
		// Clean up the newly created interpreters if validation fails
		if bn.AnalysisInterpreter != nil {
			bn.AnalysisInterpreter.Delete()
		}
		if bn.RangeInterpreter != nil {
			bn.RangeInterpreter.Delete()
		}
		// Restore the old interpreters
		bn.AnalysisInterpreter = oldAnalysisInterpreter
		bn.RangeInterpreter = oldRangeInterpreter
		bn.logger.Error("Model validation failed", "error", err, "status", "❌")
		return fmt.Errorf("model validation failed: %w", err)
	}

	// Clean up old interpreters after successful reload
	if oldAnalysisInterpreter != nil {
		oldAnalysisInterpreter.Delete()
	}
	if oldRangeInterpreter != nil {
		oldRangeInterpreter.Delete()
	}

	bn.logger.Info("Model reload completed successfully", "status", "✅")
	return nil
}

// Debug logs debug messages if debug mode is enabled
func (bn *BirdNET) Debug(message string, fields ...interface{}) {
	if bn.Settings.BirdNET.Debug {
		bn.logger.Debug(message, fields...)
	}
}

// SetLogger allows setting a custom logger for the BirdNET instance
func (bn *BirdNET) SetLogger(l *logger.Logger) {
	if l != nil {
		bn.logger = l.Named("birdnet")
	} else {
		// Fall back to global logger if nil is provided
		bn.logger = logger.Named("birdnet")
	}
}
