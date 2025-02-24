//go:build windows

package main

import (
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"os/user"
	"regexp"
	"strings"

	"golang.org/x/sys/windows"

	ntfs "www.velocidex.com/golang/go-ntfs/parser"
)

const (
	NTFSAttrType_Data = 128
	NTFSAttrID_Normal = 0
	NTFSAttrID_ADS    = 5
)

var (
	inFile                            = flag.String("in", "", "input file")
	inPath                            = flag.String("inpath", "", "input path")
	ninja                             = flag.Bool("ninja", false, "ninja mode")
	outFile                           = flag.String("out", "", "output file")
	ErrReturnedNil                    = errors.New("result returned nil reference")
	ErrInvalidInput                   = errors.New("invalid input")
	ErrDeviceInaccessible             = errors.New("raw device is not accessible")
	ErrPrivilegedAccountWanted        = errors.New("require privileged token, please uac elevate")
	SoftVersion                string = ""
)

func CheckIfElevated() error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	log.Printf("Current running as: %s (%s) ", u.Name, u.Username)
	if !windows.GetCurrentProcessToken().IsElevated() {
		return ErrPrivilegedAccountWanted
	}
	log.Println("Current process already elevated to administrator, go ahead.")
	return nil
}

func init() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)
}

func main() {
	log.Println("go-rawcopy by kmahyyg (2022) - " + SoftVersion)
	if err := CheckIfElevated(); err != nil {
		panic(err)
	}
	// Check if ninja => list folder C:\Windows\NTDS and copy each file into C:\Windows\NTDS\bkp
	if *ninja || *inPath != "" {
		ntdsPath := ""
		if *ninja {
			log.Println("Ninja mode activated.")
			ntdsPath = "C:\\Windows\\NTDS"
		} else {
			log.Println("Ninja mode activated with custom path.")
			ntdsPath = *inPath
		}
		ntdsBkpPath := ntdsPath + "\\bkp"
		// create ntdsBkpPath if missing
		if _, err := os.Stat(ntdsBkpPath); os.IsNotExist(err) {
			err = os.Mkdir(ntdsBkpPath, 0755)
			if err != nil {
				log.Fatalln(err)
			}
		}
		log.Println("Ninja mode activated. src=", ntdsPath, " dst=", ntdsBkpPath)
		ntdsFiles, err := os.ReadDir(ntdsPath)
		if err != nil {
			log.Fatalln(err)
		}
		for _, file := range ntdsFiles {
			if file.IsDir() {
				continue
			}
			ntdsFile := ntdsPath + "\\" + file.Name()
			ntdsBkpFile := ntdsBkpPath + "\\" + file.Name()
			npath := EnsureNTFSPath(ntdsFile)
			// fullpath can leave with prefixing backslash, and this library require file path in slash (*nix format)
			npathRela := strings.Join(npath[1:], "//")
			if err = TryRetrieveFile(npath[0], npathRela, ntdsBkpFile); err != nil {
				log.Printf("[!]Failled to copy %s: %v", ntdsFile, err)
			}
		}

		if *ninja {
			// backup SYSTEM and SECURITY hive
			systemFile := "C:\\Windows\\System32\\config\\SYSTEM"
			securityFile := "C:\\Windows\\System32\\config\\SECURITY"
			systemBkpFile := ntdsBkpPath + "\\SYSTEM"
			securityBkpFile := ntdsBkpPath + "\\SECURITY"

			systemPath := EnsureNTFSPath(systemFile)
			systemPathRela := strings.Join(systemPath[1:], "//")
			if err = TryRetrieveFile(systemPath[0], systemPathRela, systemBkpFile); err != nil {
				log.Printf("[!]Failed to copy SYSTEM hive: %v", err)
			}

			securityPath := EnsureNTFSPath(securityFile)
			securityPathRela := strings.Join(securityPath[1:], "//")
			if err = TryRetrieveFile(securityPath[0], securityPathRela, securityBkpFile); err != nil {
				log.Printf("[!]Failed to copy SECURITY hive: %v", err)
			}
		}

		log.Println("Ninja mode finished.")
		return
	}

	if *inFile == "" || *outFile == "" {
		log.Fatalln("Please provide input and output file.")
	}
	npath := EnsureNTFSPath(*inFile)
	// fullpath can leave with prefixing backslash, and this library require file path in slash (*nix format)
	npathRela := strings.Join(npath[1:], "//")
	err := TryRetrieveFile(npath[0], npathRela, *outFile)
	if err != nil {
		log.Fatalln(err)
	}
}

func EnsureNTFSPath(volFilePath string) []string {
	return strings.Split(volFilePath, "\\")
}

// TryRetrieveFile create a NTFSContext for specific volume and find the corresponding file,
// after found the corresponding MFT Entry, read the (ATTR(Type=16)-$STANDARD_INFORMATION)
// to retrieve file metadata. Then use OpenStream to try extract file from (ATTR(Type=128)-$DATA),
// read data via raw device require pagedReader, each read operation must fit a cluster size,
// which by default, is 4096 bytes.
func TryRetrieveFile(volDiskLetter string, filePath string, outfile string) error {
	log.Printf("Copy file %s from NTFS volume to %s.", filePath, outfile)
	log.Println("Check Drive Letter.")
	// check user input
	var IsDiskLetter = regexp.MustCompile(`^[a-zA-Z]:$`).MatchString
	if !IsDiskLetter(volDiskLetter) {
		return ErrInvalidInput
	}

	log.Println("Open Raw Device Handle.")
	// use UNC path to access raw device to bypass limitation of file lock
	volFd, err := os.Open("\\\\.\\" + volDiskLetter)
	if err != nil {
		return ErrDeviceInaccessible
	}

	log.Println("Create PagedReader with page 4096, cache size 65536.")
	// build a pagedReader for raw device to feed the NTFSContext initializor
	ntfsPagedReader, err := ntfs.NewPagedReader(volFd, 0x1000, 0x10000)
	if err != nil {
		return err
	}

	log.Println("Create NTFSContext.")
	// build NTFS context for root device
	ntfsVolCtx, err := ntfs.GetNTFSContext(ntfsPagedReader, 0)
	if err != nil {
		return err
	}

	log.Println("Start to find root directory.")
	// get volume root
	ntfsVolRoot, err := ntfsVolCtx.GetMFT(5)
	if err != nil {
		return err
	}

	log.Println("Try to find file MFT_Entry Location.")
	// ref: https://www.andreafortuna.org/2017/07/18/how-to-extract-data-and-timeline-from-master-file-table-on-ntfs-filesystem/
	// a resident file might contain multiple VCN attributes and sub-streams, use OpenStream to retrieve data
	// here, we found corresponding MFT Entry first.
	corrFileEntry, err := ntfsVolRoot.Open(ntfsVolCtx, filePath)
	if err != nil {
		return err
	}

	log.Println("Metadata checking...")
	// after found MFT_ENTRY, retrieve file metadata information located in corresponding data-stream attribute
	corrFileInfo, err := corrFileEntry.StandardInformation(ntfsVolCtx)
	if err != nil {
		return err
	}

	fulPath, err := ntfs.GetFullPath(ntfsVolCtx, corrFileEntry)
	if err != nil {
		return err
	}
	err = PrintFileMetadata(corrFileInfo, volDiskLetter+"/"+fulPath)
	if err != nil {
		return err
	}

	log.Println("Retrieving data streaming from attr.")
	// retrieve file data by read $DATA
	// check handwritten.go inside NTFS library for more details
	// ref: www.velocidex.com/golang/go-ntfs@v0.1.2-0.20221022134455-f91169ef8a39/parser/handwritten.go:63
	//
	corrFileReader, err := ntfs.OpenStream(ntfsVolCtx, corrFileEntry, NTFSAttrType_Data, NTFSAttrID_Normal)
	if err != nil {
		return err
	}

	// If there is an ADS, try read. ADS signature is filename including ":"
	// check it before you apply extractData function.
	//
	//corrFileADSReader, err := ntfs.OpenStream(ntfsVolCtx, corrFileEntry, NTFSAttrType_Data, NTFSAttrID_ADS)
	//if err != nil {
	//  return err
	//}
	//

	log.Println("Well, let's start copy now.")
	err = CopyToDestinationFile(corrFileReader, outfile)
	if err != nil {
		return err
	}

	log.Println("Copy done. Try to applying original file time.")
	err = ApplyOriginalMetadata(volDiskLetter+"/"+fulPath, corrFileInfo, *outFile)
	if err != nil {
		return err
	}

	log.Println("Workload successfully finished.")
	return nil
}

func ApplyOriginalMetadata(path string, info *ntfs.STANDARD_INFORMATION, dst string) error {
	winFileHd, err := windows.Open(dst, windows.O_RDWR, 0)
	defer windows.CloseHandle(winFileHd)
	if err != nil {
		return err
	}
	// golang official os package does not support Creation Time change due to non-POSIX complaint
	// use windows specific API only.
	cTime4Win := windows.NsecToFiletime(info.Create_time().UnixNano())
	aTime4Win := windows.NsecToFiletime(info.File_accessed_time().UnixNano())
	mTime4Win := windows.NsecToFiletime(info.File_altered_time().UnixNano())
	err = windows.SetFileTime(winFileHd, &cTime4Win, &aTime4Win, &mTime4Win)
	if err != nil {
		return err
	}
	return nil
}

func PrintFileMetadata(stdinfo *ntfs.STANDARD_INFORMATION, fullpath string) error {
	if stdinfo == nil {
		return ErrReturnedNil
	}

	log.Printf(`
    File Path: %s
    File CTime: %s
    File MTime: %s
    MFT MTime: %s
    File ATime: %s
    Size: %d
`, fullpath, stdinfo.Create_time().String(), stdinfo.File_altered_time().String(),
		stdinfo.Mft_altered_time().String(), stdinfo.File_accessed_time().String(), stdinfo.Size())
	return nil
}

func CopyToDestinationFile(src ntfs.RangeReaderAt, dstfile string) error {
	if src == nil {
		return ErrReturnedNil
	}

	log.Println("Copying to " + dstfile)
	dstFd, err := os.Create(dstfile)
	defer dstFd.Sync()
	defer dstFd.Close()
	if err != nil {
		return err
	}
	for idx, rn := range src.Ranges() {
		log.Printf("\tSplit Run %03d : Range Start From %v - Length: %v , IsSparse %v \n", idx, rn.Offset, rn.Length, rn.IsSparse)
	}

	convertedReader := ConvertFromReaderAtToReader(src, 0)

	wBytes, err := io.Copy(dstFd, convertedReader)
	log.Printf("Written %d Bytes to Destination Done. \n", wBytes)
	if err != nil {
		return err
	}

	return nil
}

type readerFromRangedReaderAt struct {
	r      io.ReaderAt
	offset int64
}

func ConvertFromReaderAtToReader(r io.ReaderAt, o int64) *readerFromRangedReaderAt {
	return &readerFromRangedReaderAt{r: r, offset: o}
}

func (rd *readerFromRangedReaderAt) Read(b []byte) (n int, err error) {
	n, err = rd.r.ReadAt(b, rd.offset)
	if n > 0 {
		rd.offset += int64(n)
	}
	return
}
