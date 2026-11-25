<?php

// PHP's own sessions, one per PHP request as under PHP-FPM.

session_start();
$_SESSION['visits'] = ($_SESSION['visits'] ?? 0) + 1;
echo 'visits=', $_SESSION['visits'], "\n";
