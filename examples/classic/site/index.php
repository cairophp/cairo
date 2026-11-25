<?php

// The front controller: every path that names no script and no file comes
// here, as WordPress' pretty links do.

echo 'front ', $_SERVER['REQUEST_URI'], ' script=', $_SERVER['SCRIPT_NAME'], "\n";
