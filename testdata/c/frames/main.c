#include <stdio.h>

struct point {
    int x;
    int y;
    const char *name;
};

struct id {
    int code;
    long stamp;
};

struct nested_inner {
    struct id fid;
    int n;
};

struct outer {
    struct nested_inner in;
    const char *tag;
};

int inner(struct point *p, int depth) {
    struct point local = {p->x + depth, p->y - depth, "local"};
    struct outer nested = {{{7, 99}, 3}, "nested"};
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
