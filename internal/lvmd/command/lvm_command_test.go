package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/go-logr/logr/testr"
	"github.com/topolvm/topolvm"
	"github.com/topolvm/topolvm/internal/lvmd/testutils"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func Test_lvm_command(t *testing.T) {
	testutils.RequireRoot(t)

	t.Run("simple lvm version should succeed with stream", func(t *testing.T) {
		ctx := log.IntoContext(context.Background(), testr.New(t))
		dataStream, err := callLVMStreamed(ctx, verbosityLVMStateNoUpdate, "version")
		if err != nil {
			t.Fatal(err, "version should succeed")
		}

		data, err := io.ReadAll(dataStream)
		if err != nil {
			t.Fatal(err, "data should be readable from io stream")
		}
		if err := dataStream.Close(); err != nil {
			t.Fatal(err, "data stream should close without problems")
		}
		if !strings.Contains(string(data), "LVM version") {
			t.Fatal("LVM version not found in output")
		}
	})

	t.Run("simple lvm vgs should return not found but other failures should not", func(t *testing.T) {
		var messages []string
		funcLog := funcr.New(func(_, args string) {
			messages = append(messages, args)
		}, funcr.Options{
			Verbosity: 9,
		})

		ctx := log.IntoContext(context.Background(), funcLog)

		err := callLVM(ctx, "vgs", "non-existing-vg")
		if err == nil {
			t.Fatal(err, "vg should not exist")
		}

		if !IsLVMNotFound(err) {
			t.Fatal("error should be not found")
		}

		if len(messages) != 1 || !strings.Contains(messages[0], "invoking command") {
			t.Fatal("there should be nothing in stdout except the invoking command log")
		}

		err = callLVM(ctx, "foobar")
		if err == nil {
			t.Fatal(err, "command should not be recognized")
		}

		if IsLVMNotFound(err) {
			t.Fatal("error should not be not found")
		}
	})

	t.Run("simple lvm vgcreate with non existing device should fail and show logs", func(t *testing.T) {
		// fakeDeviceName is a string that does not exist on the system (or rather is highly unlikely to exist)
		// it is used to test the error handling of the callLVMStreamed function
		fakeDeviceName := "/dev/does-not-exist"

		ctx := log.IntoContext(context.Background(), testr.New(t))
		dataStream, err := callLVMStreamed(ctx, verbosityLVMStateUpdate, "vgcreate", "test-vg", fakeDeviceName)
		if err != nil {
			t.Fatal(err, "vgcreate should not fail instantly as read didn't finish")
		}
		data, err := io.ReadAll(dataStream)
		if err != nil {
			t.Fatal(err, "data should be readable from io stream")
		}
		if len(data) != 0 {
			t.Fatal("data should be empty as the command should fail")
		}
		err = dataStream.Close()
		if err == nil {
			t.Fatal(err, "data stream should fail during close")
		}

		lvmErr, ok := AsLVMError(err)
		if !ok {
			t.Fatal("error should be a LVM error")
		}
		if lvmErr == nil {
			t.Fatal("error should not be nil")
		}
		if lvmErr.ExitCode() != 5 {
			t.Fatalf("exit code should be 5, got %d", lvmErr.ExitCode())
		}
		if !strings.Contains(lvmErr.Error(), "exit status 5") {
			t.Fatal("exit status 5 not contained in error")
		}
		if !strings.Contains(lvmErr.Error(), fmt.Sprintf("No device found for %s", fakeDeviceName)) {
			t.Fatal("No device found message not contained in error")
		}
	})

	t.Run("callLVM should succeed for non-json based calls", func(t *testing.T) {
		var messages []string
		funcLog := funcr.New(func(_, args string) {
			messages = append(messages, args)
		}, funcr.Options{
			Verbosity: 9,
		})

		ctx := log.IntoContext(context.Background(), funcLog)
		err := callLVM(ctx, "version")
		if err != nil {
			t.Fatal(err, "version should succeed")
		}

		if len(messages) == 0 {
			t.Fatal("no messages logged")
		}

		match, _ := regexp.MatchString(`"args"=\[.* "/sbin/lvm" "version"\]`, messages[0])
		if !match {
			t.Fatal("command log was not found")
		}

		// check if the version message was logged
		stdoutExistsInLogs := false
		for _, m := range messages[1:] {
			if strings.Contains(m, "LVM version") {
				stdoutExistsInLogs = true
				break
			}
		}
		if !stdoutExistsInLogs {
			t.Fatalf("version from stdout was not logged, existing logs: %v", messages)
		}
	})

	t.Run("lv creation", func(t *testing.T) {
		ctx := ctrl.LoggerInto(context.Background(), testr.New(t))
		vgName := "lvm_command_test"
		loop, err := testutils.MakeLoopbackDevice(ctx, vgName)
		if err != nil {
			t.Fatal(err)
		}

		err = testutils.MakeLoopbackVG(ctx, vgName, loop)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = testutils.CleanLoopbackVG(vgName, []string{loop}, []string{vgName}) }()

		vg, err := FindVolumeGroup(ctx, vgName)
		if err != nil {
			t.Fatal(err)
		}

		t.Run("create volume with multiple of Sector Size is fine", func(t *testing.T) {
			err = vg.CreateVolume(ctx, "test1", uint64(topolvm.MinimumSectorSize), []string{"tag"}, 0, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			vol, err := vg.FindVolume(ctx, "test1")
			if err != nil {
				t.Fatal(err)
			}
			if vol.Size()%uint64(topolvm.MinimumSectorSize) != 0 {
				t.Fatalf("expected size to be multiple of sector size %d, got %d", uint64(topolvm.MinimumSectorSize), vol.Size())
			}
			if err := vg.RemoveVolume(ctx, "test1"); err != nil {
				t.Fatal(err)
			}
		})

		t.Run("create volume with size not multiple of sector Size to get rejected", func(t *testing.T) {
			err = vg.CreateVolume(ctx, "test1", uint64(topolvm.MinimumSectorSize)+1, []string{"tag"}, 0, "", nil)
			if !errors.Is(err, ErrNoMultipleOfSectorSize) {
				t.Fatalf("expected error to be %v, got %v", ErrNoMultipleOfSectorSize, err)
			}
		})

		t.Run("create cached volume and it should not classified as thin volume.", func(t *testing.T) {
			// create the cachedevice
			cache_vg_name := "CACHEDEVICE"
			cache_loop, err := testutils.MakeLoopbackDevice(ctx, cache_vg_name)
			if err != nil {
				t.Fatal(err)
			}
			err = testutils.MakeLoopbackVG(ctx, cache_vg_name, cache_loop)
			if err != nil {
				t.Fatal(err)
			}
			// ensure cache device cleanup
			defer func() { _ = testutils.CleanLoopbackVG(cache_vg_name, []string{cache_loop}, []string{cache_vg_name}) }()
			vg, err := FindVolumeGroup(ctx, cache_vg_name)
			if err != nil {
				t.Fatal(err)
			}
			// create cached LV
			err = vg.CreateVolume(ctx, "test1", uint64(topolvm.MinimumSectorSize), []string{"tag"}, 0, "", []string{"--type", "writecache", "--cachesize", "10M", "--cachedevice", cache_loop})
			if err != nil {
				t.Fatal(err)
			}
			vol, err := vg.FindVolume(ctx, "test1")
			if err != nil {
				t.Fatal(err)
			}
			if vol.IsThin() {
				t.Fatal("Expected test1 to not be thin (eval lv_attr instead of pool)")
			}
			if vol.attr[0] != byte(VolumeTypeCached) {
				t.Fatal("Created a LV but without writecache?")
			}
		})
	})
}
