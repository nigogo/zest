(function () {
  function createQRCodeScanner(options) {
    var video = options.video;
    var status = options.status;
    var startButton = options.startButton;
    var stopButton = options.stopButton;
    var fileInput = options.fileInput;
    var onScanSuccess = options.onScanSuccess;
    var onScanError = options.onScanError;
    var messages = options.messages || {};
    var stream = null;
    var scanning = false;
    var completed = false;
    var frameHandle = 0;
    var detector = null;
    var canvas = document.createElement("canvas");
    var ctx = canvas.getContext("2d", { willReadFrequently: true });

    function message(key, fallback) {
      return messages[key] || fallback;
    }

    function setStatus(key, fallback, cls) {
      if (!status) return;
      status.textContent = message(key, fallback);
      status.className = "status-banner " + (cls || "status-info");
    }

    function showStartButton(show) {
      if (!startButton) return;
      startButton.hidden = !show;
      startButton.disabled = !show;
    }

    function stopScanner() {
      scanning = false;
      if (frameHandle) {
        cancelAnimationFrame(frameHandle);
        frameHandle = 0;
      }
      if (stream) {
        // Always release the camera; mobile browsers otherwise keep recording in the background.
        stream.getTracks().forEach(function (track) { track.stop(); });
        stream = null;
      }
      if (video) video.srcObject = null;
      if (!completed) showStartButton(true);
    }

    function fail(key, fallback, error) {
      setStatus(key, fallback, "status-warning");
      stopScanner();
      if (typeof onScanError === "function") onScanError(error || new Error(fallback));
    }

    function createBarcodeDetector() {
      if (detector !== null) return detector;
      detector = false;
      if (typeof BarcodeDetector !== "function") return detector;
      try {
        detector = new BarcodeDetector({ formats: ["qr_code"] });
      } catch (error) {
        detector = false;
      }
      return detector;
    }

    function decodeWithJsQR() {
      if (typeof jsQR !== "function") return null;
      if (!ctx || !video || video.readyState < 2 || !video.videoWidth || !video.videoHeight) return null;
      var max = 720;
      var scale = Math.min(1, max / Math.max(video.videoWidth, video.videoHeight));
      canvas.width = Math.max(1, Math.floor(video.videoWidth * scale));
      canvas.height = Math.max(1, Math.floor(video.videoHeight * scale));
      ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
      var code = jsQR(ctx.getImageData(0, 0, canvas.width, canvas.height).data, canvas.width, canvas.height, { inversionAttempts: "attemptBoth" });
      return code && code.data ? code.data : null;
    }

    async function decodeCurrentFrame() {
      var barcodeDetector = createBarcodeDetector();
      if (barcodeDetector) {
        try {
          var barcodes = await barcodeDetector.detect(video);
          if (barcodes && barcodes[0] && (barcodes[0].rawValue || barcodes[0].data)) {
            return barcodes[0].rawValue || barcodes[0].data;
          }
        } catch (error) {
          detector = false;
        }
      }
      return decodeWithJsQR();
    }

    function finish(result) {
      if (completed) return;
      completed = true;
      stopScanner();
      setStatus("found", "QR code found. Opening scan…", "status-success");
      if (typeof onScanSuccess === "function") onScanSuccess(result);
    }

    async function scanFrame() {
      if (!scanning || completed) return;
      try {
        var result = await decodeCurrentFrame();
        if (result) {
          finish(result);
          return;
        }
        setStatus("none", "No QR code detected yet", "status-info");
      } catch (error) {
        setStatus("failed", "Scan failed, try again", "status-warning");
      }
      frameHandle = requestAnimationFrame(scanFrame);
    }

    async function startScanner() {
      if (stream || scanning) return;
      completed = false;
      if (!window.isSecureContext) {
        fail("insecure", "Camera unavailable", new Error("Camera requires a secure context"));
        return;
      }
      if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
        fail("unavailable", "Camera unavailable", new Error("getUserMedia is not supported"));
        return;
      }
      if (!createBarcodeDetector() && typeof jsQR !== "function") {
        fail("failed", "Scan failed, try again", new Error("QR decoder is not available"));
        return;
      }
      setStatus("starting", "Starting camera…", "status-info");
      showStartButton(false);
      try {
        stream = await navigator.mediaDevices.getUserMedia({ video: { facingMode: { ideal: "environment" } }, audio: false });
        if (!stream || stream.getTracks().length === 0) {
          fail("unavailable", "Camera unavailable", new Error("No camera tracks available"));
          return;
        }
        video.srcObject = stream;
        // iOS Safari requires inline playback for camera preview instead of forcing fullscreen video.
        video.setAttribute("playsinline", "");
        video.playsInline = true;
        await video.play();
        scanning = true;
        setStatus("scanning", "Scanning…", "status-info");
        frameHandle = requestAnimationFrame(scanFrame);
      } catch (error) {
        var denied = error && (error.name === "NotAllowedError" || error.name === "PermissionDeniedError");
        fail(denied ? "denied" : "unavailable", denied ? "Camera permission denied" : "Camera unavailable", error);
      }
    }

    function decodeImageFile(file) {
      if (!file || typeof jsQR !== "function" || !ctx) return;
      stopScanner();
      setStatus("scanning", "Scanning…", "status-info");
      var img = new Image();
      img.onload = function () {
        try {
          var max = 1200;
          var scale = Math.min(1, max / Math.max(img.naturalWidth, img.naturalHeight));
          canvas.width = Math.max(1, Math.floor(img.naturalWidth * scale));
          canvas.height = Math.max(1, Math.floor(img.naturalHeight * scale));
          ctx.drawImage(img, 0, 0, canvas.width, canvas.height);
          var code = jsQR(ctx.getImageData(0, 0, canvas.width, canvas.height).data, canvas.width, canvas.height, { inversionAttempts: "attemptBoth" });
          URL.revokeObjectURL(img.src);
          if (code && code.data) finish(code.data);
          else fail("none", "No QR code detected yet", new Error("No QR code found in uploaded image"));
        } catch (error) {
          fail("failed", "Scan failed, try again", error);
        }
      };
      img.onerror = function () { fail("failed", "Scan failed, try again", new Error("Could not load QR image")); };
      img.src = URL.createObjectURL(file);
    }

    if (startButton) startButton.addEventListener("click", startScanner);
    if (stopButton) stopButton.addEventListener("click", stopScanner);
    if (fileInput) fileInput.addEventListener("change", function () { decodeImageFile(fileInput.files && fileInput.files[0]); });
    window.addEventListener("pagehide", stopScanner);
    window.addEventListener("beforeunload", stopScanner);

    return { startScanner: startScanner, stopScanner: stopScanner, onScanSuccess: onScanSuccess, onScanError: onScanError };
  }

  window.ZestQRScanner = { create: createQRCodeScanner };
})();
