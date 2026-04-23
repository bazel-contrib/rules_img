#include <iostream>
#include <fstream>
#include "rules_cc/cc/runfiles/runfiles.h"

using rules_cc::cc::runfiles::Runfiles;

// BAZEL_CURRENT_REPOSITORY is not defined in Bazel 7 WORKSPACE mode.
#ifndef BAZEL_CURRENT_REPOSITORY
#define BAZEL_CURRENT_REPOSITORY "_main"
#endif

int main(int argc, char* argv[]) {
    std::string error;
    std::unique_ptr<Runfiles> runfiles(
    Runfiles::Create(argv[0], BAZEL_CURRENT_REPOSITORY, &error));

    if (runfiles == nullptr) {
        std::cerr << "Unable to locate runfiles" << std::endl;
        return 1;
    }
    std::string greeting_path =
        runfiles->Rlocation("_main/image_from_binary_example/greeting.txt");

    std::ifstream f(greeting_path);

    if (f.is_open()) {
        std::cout << f.rdbuf();
    } else {
        std::cerr << "Unable to read greeting" << std::endl;
        return 1;
    }
	return 0;
}
