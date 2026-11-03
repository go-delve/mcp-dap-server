#include <unistd.h>

int main(void) {
	volatile int x = 0;
	for (;;) {
		x++;
		usleep(10000);
	}
}
