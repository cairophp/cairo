<?php

// A fatal error ends this PHP request, as under PHP-FPM; the thread takes
// the next request in a fresh one.

echo "partial\n";
no_such_function();
