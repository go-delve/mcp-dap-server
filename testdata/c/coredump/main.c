#include <stdlib.h>

void crash(int x, const char *msg) {
    (void)x;
    (void)msg;
    abort();
}

int main() {
    crash(42, "core dump test");
    return 0;
}
