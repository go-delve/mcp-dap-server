#include <stdio.h>

struct point {
    int x;
    int y;
    const char *name;
};

int inner(struct point *p, int depth) {
    struct point local = {p->x + depth, p->y - depth, "local"};
    if (depth > 0) {
        return inner(&local, depth - 1);
    }
    printf("%d\n", local.x);
    return local.x;
}

int middle(struct point *p) {
    return inner(p, 2);
}

int main(void) {
    struct point origin = {10, 20, "origin"};
    return middle(&origin);
}
