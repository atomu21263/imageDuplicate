package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/corona10/goimagehash"
	"golang.org/x/crypto/sha3"
)

// コンフィグ
type checkConfig struct {
	Ffmpeg      string   `json:"ffmpeg"`
	Search      []string `json:"search"`
	PhotoAccept int      `json:"photoAccept"`
	VideoAccept int      `json:"videoAccept"`
	QueueLimit  int      `json:"queueLimit"`
}

// カウンター
type FilesInfo struct {
	valueB, valueKB, valueMB, valueGB, valueTB int
	ImageFileCount                             int
	VideoFileCount                             int
	DirCount                                   int
}

// hash用struct
type photoHash struct {
	imgHash       *goimagehash.ImageHash
	sha512        string
	path          string
	width, height int
}

type videoHash struct {
	imgHashs      [3]*goimagehash.ImageHash
	sha512        string
	path          string
	width, height int
	time          int
}

// json用struct
type JsonExport struct {
	Duplicate []ImageInfoDuplicate `json:"duplicate"`
	Similar   []ImageInfoSimilar   `json:"similar"`
	Other     []ImageInfoOther     `json:"other"`
}

type ImageInfoDuplicate struct {
	Compare CompareImageData `json:"compare"`
	With    []string         `json:"withs"`
}

type ImageInfoSimilar struct {
	Compare CompareImageData       `json:"compare"`
	With    []WithImageDataSimilar `json:"with"`
}

type ImageInfoOther struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

type CompareImageData struct {
	Path   string `json:"path"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type WithImageDataSimilar struct {
	Path     string `json:"path"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Distance int    `json:"distance"`
}

var (
	configPath = flag.String("config", "./config.json", "Search Options Json File")
	config     = checkConfig{}
	photoFiles = []photoHash{}
	videoFiles = []videoHash{}
	info       = FilesInfo{}
	funcs      sync.WaitGroup // goroutienの終了待機用
	mapEdit    sync.Mutex
	result     = JsonExport{}
	errors     = []error{}
)

func init() {
	flag.Parse()
	configFile, _ := os.Open(*configPath)
	defer configFile.Close()
	configBytes, _ := io.ReadAll(configFile)
	json.Unmarshal(configBytes, &config)
}

func main() {
	// コンフィグ表示
	fmt.Println("-----[Config]-----")
	fmt.Printf("ffmpeg      : %s\n", config.Ffmpeg)
	fmt.Printf("search      : %s\n", config.Search)
	fmt.Printf("photoAccept : %d\n", config.PhotoAccept)
	fmt.Printf("videoAccept : %d\n", config.VideoAccept)
	fmt.Printf("queueLimit  : %d\n", config.QueueLimit)
	time.Sleep(5 * time.Second)

	// ffmpegチェック
	err := exec.Command(config.Ffmpeg, "-h").Run()
	if err != nil {
		fmt.Println(err)
		panic("Error: Failed Run ffmpeg!")
	}

	// search
	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("Start File Scan")
	fmt.Println("----------------------------------------")
	fmt.Println("")
	queue := make(chan struct{}, config.QueueLimit) // 並列上限
	for _, searchDir := range config.Search {
		filepath.WalkDir(searchDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				fmt.Println(err)
				panic("Error: Failed Walk Directorys!")
			}
			// ディレクトリチェック
			index := strings.Count(path, string(os.PathSeparator))
			if d.IsDir() {
				fmt.Printf("%sDirectory: %s(%s)\n", Space(index-1), d.Name(), path)
				info.DirCount++
				fmt.Printf("%sNow Used: %dT %dG %dM %dK %dB %dDir %dFiles\n", Space(index-1), info.valueTB, info.valueGB, info.valueMB, info.valueKB, info.valueB, info.DirCount, info.ImageFileCount+info.VideoFileCount)
				return nil
			}
			// ファイルサイズ
			fileInfo, _ := os.Stat(path)
			ValueSize(fileInfo.Size())

			// 並列処理
			queue <- struct{}{} // queueの追加
			funcs.Add(1)        // 実行追加
			go func(pathFunc string, fileInfoFunc fs.FileInfo, infoFunc FilesInfo) {
				defer funcs.Done()
				defer func() { <-queue }()
				// ファイル処理
				fileExt := strings.ToLower(pathFunc)
				fileExt = filepath.Ext(fileExt)

				// 画像(jpeg jpg png webp jfif) の処理
				if strings.Contains(".jpeg .jpg .png .webp .jfif", fileExt) {
					// ファイル数追加
					info.ImageFileCount++
					fmt.Printf("%s ・%-40s  (%s) Image No.%04d\n", Space(index), d.Name(), Size(fileInfoFunc.Size()), info.ImageFileCount)
					// Hash化
					img, imgHash, err := image2Hash(pathFunc)
					if err != nil {
						errors = append(errors, err)
						return
					}
					sha512, err := sha3_512(pathFunc)
					if err != nil {
						errors = append(errors, err)
						return
					}

					photoFiles = append(photoFiles, photoHash{
						imgHash: imgHash,
						sha512:  sha512,
						path:    pathFunc,
						width:   img.Bounds().Dx(),
						height:  img.Bounds().Dy(),
					})
					return
				}

				// 映像(mp4 mov webm) の処理
				if strings.Contains(".mp4 .mov .webm", fileExt) {
					// ファイル数追加
					info.VideoFileCount++
					fmt.Printf("%s ・%-40s  (%s) Video No.%04d\n", Space(index), d.Name(), Size(fileInfoFunc.Size()), info.VideoFileCount)
					// 動画データ入手
					video := videoHash{
						path:     pathFunc,
						imgHashs: [3]*goimagehash.ImageHash{},
					}
					videoTime := 0
					out, _ := exec.Command(config.Ffmpeg, "-i", pathFunc).CombinedOutput()
					for _, line := range strings.Split(string(out), "\n") {
						// 動画時間入手
						if strings.Contains(line, "Duration") {
							line = regexp.MustCompile(".*([0-9]{2}):([0-9]{2}):([0-9]{2}).*").ReplaceAllString(line, "$1 $2 $3")
							var hour, min, sec int
							fmt.Sscanf(line, "%d %d %d", &hour, &min, &sec)
							videoTime = hour*3600 + min*60 + sec
						}
						// 動画の画質入手
						if strings.Contains(line, ": Video:") {
							line = regexp.MustCompile(".+?([0-9]{2,5})x([0-9]{2,5}).*").ReplaceAllString(line, "$1 $2")
							fmt.Sscanf(line, "%d %d", &video.width, &video.height)
						}
					}
					// 動画のスクショを入手
					videoPhotoTiming := videoTime / 3
					videoPhotoTimingOffset := videoPhotoTiming / 2
					for i := 0; i < 3; i++ {
						// dirチェック
						if _, err := os.Stat("./temp"); err != nil {
							err := os.Mkdir("./temp", 0666)
							if err != nil {
								panic(err)
							}
						}
						// 取り出し
						timing := fmt.Sprintf("%d", videoPhotoTiming*i-videoPhotoTimingOffset)
						tempPhoto := fmt.Sprintf("./temp/%s_videoPhoto.png", fileInfoFunc.Name())
						exec.Command(config.Ffmpeg, "-ss", timing, "-i", pathFunc, "-frames:v", "1", tempPhoto).Run()
						// Hash保存
						_, imghash, err := image2Hash(fmt.Sprintf("./temp/%s_videoPhoto.png", fileInfoFunc.Name()))
						if err != nil {
							errors = append(errors, err)
							return
						}
						video.imgHashs[i] = imghash
					}
					// temp写真削除
					os.Remove(fmt.Sprintf("./temp/%s_videoPhoto.png", fileInfoFunc.Name()))

					// sha3-512生成
					sha512, err := sha3_512(pathFunc)
					if err != nil {
						errors = append(errors, err)
						return
					}
					video.sha512 = sha512

					// 保存
					mapEdit.Lock()
					defer mapEdit.Unlock()
					videoFiles = append(videoFiles, video)
					return
				}
				fmt.Printf("%s ・%-40s  (%s)                Skip(%s)\n", Space(index), d.Name(), Size(fileInfoFunc.Size()), fileExt)
			}(path, fileInfo, info)
			return nil
		})
	}
	funcs.Wait()

	fmt.Println("")
	fmt.Printf("Scaned File: %dT %dG %dM %dK %dB %dDir %dFiles(%dPhoto, %dVideo)\n", info.valueTB, info.valueGB, info.valueMB, info.valueKB, info.valueB, info.DirCount, info.ImageFileCount+info.VideoFileCount, len(photoFiles), len(videoFiles))
	fmt.Println("----------------------------------------")
	fmt.Println("File Scan End")
	fmt.Println("----------------------------------------")
	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("Check Start Duplicate/Similar Files")
	fmt.Println("----------------------------------------")
	fmt.Println("")

	// 重複チェック 画像
	fmt.Println("-----[Check Duplicate Photo]-----")
	for i := 0; i < len(photoFiles); i++ {
		data1 := photoFiles[i]
		var duplicates []string
		for j := i + 1; j < len(photoFiles); j++ {
			data2 := photoFiles[j]
			// Path Check
			if data1.path == data2.path {
				continue
			}
			if data1.sha512 == data2.sha512 {
				// json用に保存
				duplicates = append(duplicates, data2.path)
				// 引っかかったのは今後検索に掛けない
				photoFiles = append(photoFiles[:j], photoFiles[j+1:]...)
			}
		}
		// 重複があればjsonに保存
		if len(duplicates) > 0 {
			result.Duplicate = append(result.Duplicate, ImageInfoDuplicate{
				Compare: CompareImageData{
					Path:   data1.path,
					Width:  data1.width,
					Height: data1.height,
				},
				With: duplicates,
			})
			// 見やすく表示
			fmt.Printf("    Duplicate\n")
			fmt.Printf("        Compare: [%4dpx*%4dpx] %s \n", data1.width, data1.height, data1.path)
			for j := 0; j < len(duplicates); j++ {
				fmt.Printf("            %s\n", duplicates[j])
			}
			photoFiles = append(photoFiles[:i], photoFiles[i+1:]...)
		}
	}
	// 重複チェック 動画
	fmt.Println("-----[Check Duplicate Video]-----")
	for i := 0; i < len(videoFiles); i++ {
		data1 := videoFiles[i]
		var duplicates []string
		for j := i + 1; j < len(videoFiles); j++ {
			data2 := videoFiles[j]
			// Path Check
			if data1.path == data2.path {
				continue
			}
			// Time Check
			if data1.time != data2.time {
				continue
			}
			if data1.sha512 == data2.sha512 {
				// json用に保存
				duplicates = append(duplicates, data2.path)
				// 引っかかったのは今後検索に掛けない
				videoFiles = append(videoFiles[:j], videoFiles[j+1:]...)
			}
		}
		// 重複があればjsonに保存
		if len(duplicates) > 0 {
			result.Duplicate = append(result.Duplicate, ImageInfoDuplicate{
				Compare: CompareImageData{
					Path:   data1.path,
					Width:  data1.width,
					Height: data1.height,
				},
				With: duplicates,
			})
			// 見やすく表示
			fmt.Printf("    Duplicate\n")
			fmt.Printf("        Compare: [%4dpx*%4dpx] %s \n", data1.width, data1.height, data1.path)
			for j := 0; j < len(duplicates); j++ {
				fmt.Printf("            %s\n", duplicates[j])
			}
			videoFiles = append(videoFiles[:i], videoFiles[i+1:]...)
		}
	}

	// 類似チェック 画像
	fmt.Println("-----[Check Similar Photo]-----")
	for i := 0; i < len(photoFiles); i++ {
		data1 := photoFiles[i]
		var duplicates []WithImageDataSimilar
		for j := i + 1; j < len(photoFiles); j++ {
			data2 := photoFiles[j]
			// Path Check
			if data1.path == data2.path {
				continue
			}
			distance, _ := data1.imgHash.Distance(data2.imgHash)
			if distance <= config.PhotoAccept {
				// json用に保存
				duplicates = append(duplicates, WithImageDataSimilar{
					Path:     data2.path,
					Width:    data2.width,
					Height:   data2.height,
					Distance: distance,
				})
				// 引っかかったのは今後検索に掛けない
				photoFiles = append(photoFiles[:j], photoFiles[j+1:]...)
			}
		}
		// 重複があればjsonに保存
		if len(duplicates) > 0 {
			result.Similar = append(result.Similar, ImageInfoSimilar{
				Compare: CompareImageData{
					Path:   data1.path,
					Width:  data1.width,
					Height: data1.height,
				},
				With: duplicates,
			})
			// 見やすく表示
			fmt.Printf("    Similar\n")
			fmt.Printf("        Compare: [%4dpx*%4dpx] %s \n", data1.width, data1.height, data1.path)
			for j := 0; j < len(duplicates); j++ {
				fmt.Printf("            [%4dpx*%4dpx] Distance:%-3d %s\n", duplicates[j].Width, duplicates[j].Height, duplicates[j].Distance, duplicates[j].Path)
			}
			photoFiles = append(photoFiles[:i], photoFiles[i+1:]...)
		}
	}
	// 類似チェック 動画
	fmt.Println("-----[Check Similar Video]-----")
	for i := 0; i < len(videoFiles); i++ {
		data1 := videoFiles[i]
		var duplicates []WithImageDataSimilar
		for j := i + 1; j < len(videoFiles); j++ {
			data2 := videoFiles[j]
			// Path Check
			if data1.path == data2.path {
				continue
			}
			// Time Check
			if data1.time != data2.time {
				continue
			}

			distance := 0
			for k := 0; k < 3; k++ {
				imageDistance, _ := data1.imgHashs[k].Distance(data2.imgHashs[k])
				distance += imageDistance
			}
			if distance <= config.VideoAccept {
				// json用に保存
				duplicates = append(duplicates, WithImageDataSimilar{
					Path:     data2.path,
					Width:    data2.width,
					Height:   data2.height,
					Distance: distance,
				})
				// 引っかかったのは今後検索に掛けない
				videoFiles = append(videoFiles[:j], videoFiles[j+1:]...)
			}
		}
		// 重複があればjsonに保存
		if len(duplicates) > 0 {
			result.Similar = append(result.Similar, ImageInfoSimilar{
				Compare: CompareImageData{
					Path:   data1.path,
					Width:  data1.width,
					Height: data1.height,
				},
				With: duplicates,
			})
			// 見やすく表示
			fmt.Printf("    Similar\n")
			fmt.Printf("        Compare: [%4dpx*%4dpx] %s \n", data1.width, data1.height, data1.path)
			for j := 0; j < len(duplicates); j++ {
				fmt.Printf("            [%4dpx*%4dpx] Distance:%-3d %s\n", duplicates[j].Width, duplicates[j].Height, duplicates[j].Distance, duplicates[j].Path)
			}
		}
		videoFiles = append(videoFiles[:i], videoFiles[i+1:]...)
	}

	// Hashを保存
	for i := 0; i < len(photoFiles); i++ {
		photo := photoFiles[i]
		result.Other = append(result.Other, ImageInfoOther{
			Path: photo.path,
			Hash: photo.sha512,
		})
	}
	for i := 0; i < len(videoFiles); i++ {
		video := videoFiles[i]
		result.Other = append(result.Other, ImageInfoOther{
			Path: video.path,
			Hash: video.sha512,
		})
	}
	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("Check End Duplicate/Similar Files")
	fmt.Println("----------------------------------------")
	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("Save Start Duplicate/Similar Data To Json")
	fmt.Println("----------------------------------------")
	fmt.Println("")

	fmt.Println("-----[Data Formatting Start (To Json)]-----")
	resultBytes, _ := json.MarshalIndent(result, "", "  ")

	fmt.Println("-----[Data Formatting End]-----")
	fmt.Println("-----[Writing Start To \"duplicate.json\"]-----")
	// 書き込み
	jsonFile, _ := os.Create("./duplicate.json")
	defer jsonFile.Close()
	writer := bufio.NewWriter(jsonFile)
	writer.Write(resultBytes)
	writer.Flush()
	fmt.Println("-----[Writing End To \"duplicate.json\"]-----")

	fmt.Println("")
	fmt.Println("----------------------------------------")
	fmt.Println("Save End Duplicate/Similar Data To Json")
	fmt.Println("----------------------------------------")
	fmt.Println("")

	// エラー表示
	if len(errors) > 0 {
		fmt.Println("Errors")
		for _, v := range errors {
			fmt.Println(v.Error())
		}
	}
}

func Space(n int) (spacer string) {
	for i := 0; i < n; i++ {
		spacer += " "
	}
	return
}

func Size(n int64) (size string) {
	// 接頭辞表記
	x := float32(n)
	if x < 1000 {
		return fmt.Sprintf("%7.2f B", x)
	} else if x < 1000*1000 {
		return fmt.Sprintf("%7.2fKB", x/1000)
	} else if x < 1000*1000*1000 {
		return fmt.Sprintf("%7.2fMB", x/1000/1000)
	} else if x < 1000*1000*1000*1000 {
		return fmt.Sprintf("%7.2fGB", x/1000/1000/1000)
	}
	return fmt.Sprintf("%7.2fTB", x/1000/1000/1000/1000)
}

func ValueSize(n int64) {
	info.valueB += int(n)
	if info.valueB >= 1000 {
		info.valueKB += info.valueB / 1000
		info.valueB = info.valueB % 1000
	}
	if info.valueKB >= 1000 {
		info.valueMB += info.valueKB / 1000
		info.valueKB = info.valueKB % 1000
	}
	if info.valueMB >= 1000 {
		info.valueGB += info.valueMB / 1000
		info.valueMB = info.valueMB % 1000
	}
	if info.valueGB >= 1000 {
		info.valueTB += info.valueGB / 1000
		info.valueGB = info.valueGB % 1000
	}
}

func image2Hash(file string) (img image.Image, imgHash *goimagehash.ImageHash, err error) {
	// 読み取り
	var imgFile *os.File
	imgFile, err = os.Open(file)
	if err != nil {
		return
	}
	defer imgFile.Close()
	// ImageHash化
	img, _, err = image.Decode(imgFile)
	if err != nil {
		return
	}
	imgHash, _ = goimagehash.PerceptionHash(img)
	return
}

func sha3_512(file string) (sha512 string, err error) {
	// 読み取り
	var imgFile []byte
	imgFile, err = os.ReadFile(file)
	if err != nil {
		return
	}
	// CryptoHash化
	sha512 = fmt.Sprintf("%x", sha3.Sum512(imgFile))
	return
}
