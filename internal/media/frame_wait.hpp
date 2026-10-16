#ifndef RELAYNET_FRAME_WAIT_HPP
#define RELAYNET_FRAME_WAIT_HPP
#include <windows.h>

// Native Sleep(1) can round up to a whole Windows timer tick. A bounded,
// high resolution wait keeps capture and GPU event polling within 120 FPS.
class FrameWait {
public:
    FrameWait() : timer_(CreateWaitableTimerExW(nullptr, nullptr, 0x00000002,
                                               TIMER_MODIFY_STATE | SYNCHRONIZE)) {}
    ~FrameWait() { if (timer_) CloseHandle(timer_); }
    FrameWait(const FrameWait&) = delete;
    FrameWait& operator=(const FrameWait&) = delete;

    void wait() {
        LARGE_INTEGER due;
        due.QuadPart = -5000; // 0.5 ms, relative, in 100 ns units.
        if (timer_ && SetWaitableTimer(timer_, &due, 0, nullptr, nullptr, FALSE)) {
            WaitForSingleObject(timer_, 10);
        } else {
            Sleep(1); // Older Windows without high resolution timer support.
        }
    }

private:
    HANDLE timer_;
};
#endif
